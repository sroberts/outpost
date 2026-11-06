# Design: AI models for Outpost's apps

Status: phase 1 (Ollama) built in v0.5.0; phase 2 (hosted providers and app
access, [#120](https://github.com/sroberts/outpost/issues/120)) built on
branch `feat/ai-providers`. The operator's side is in the README under
[AI models](../../README.md#ai-models), and the app's side in
`outpost-service-spec.md` §15. Phase 2 is described at the end; the rest of
this note is phase 1 as designed, with what changed in building it here.

**What changed in building phase 1:**

- **Tokens are derived, not stored.** Keeping only a token's hash, as
  proposed, would leave nothing to hand an app when it starts again after a
  daemon restart. A token is instead an HMAC of the app's name and a
  generation, under a key kept in `ai.db`. Regenerating bumps the generation.
  Nothing readable is stored, and the same token comes back every time.
- **The variables ride on the secrets path.** The supervisor already asks its
  `Secrets` for each app's variables, and restarts the app when their revision
  changes. `serve` wraps the secrets store, so it also returns
  `OUTPOST_AI_URL` and `OUTPOST_AI_TOKEN`, with the token's generation in the
  revision. Regenerating restarts the app with no change to the supervisor.
- **A missing model is learned from Ollama's 404.** A model missing
  from Ollama comes back from Ollama as a 404, which the proxy turns into the
  503 naming the model, instead of listing Ollama's models on every request.
- **The proxy is hand-written, not `httputil.ReverseProxy`.** The body is
  rewritten anyway, and only `Content-Type`, `Accept` and the
  `Anthropic-Version`/`-Beta` headers go upstream. Credentials never do.
- **Tokens counted include the prompt cache.** Anthropic's `input_tokens`
  leaves out cached input, and Ollama reports it there, so
  `cache_read_input_tokens` is added back to match OpenAI's
  `prompt_tokens`.

## Summary

Apps under Outpost should be able to use a model without each one choosing,
configuring and pointing at its own. Models live in one place, and Outpost
decides which ones apps get. An installation has exactly two:

- **the LLM**, a model that generates text, such as Gemma 4 or Qwen 3;
- **the System 1 model**, a decision model that answers typed questions about
  an input with probabilities, such as Laya.

In phase 1 both are served by **Ollama**, on this machine or one the operator
points at. The operator picks each from the models Ollama actually has, either
on an admin-only **AI models** page in the portal or in `config.toml`. The
portal wins, as with Funnel.

Apps don't talk to Ollama directly. Outpost serves one local endpoint, and
every app is started with its address and a token. A request for the LLM goes
to whichever model is the LLM at that moment, so changing the model on the
page takes effect on the next request, without restarting or reconfiguring
any app.

## The two kinds

**LLM.** An app calls it with the OpenAI Chat Completions API or the
Anthropic Messages API, whichever its SDK speaks. Ollama serves both.

**System 1.** `POST /v1/systemone`. The request is a `state` (text or JSON)
and up to 64 typed `questions`: `choice`, `score`, `noul` (yes/no) and `box`.
One pass returns `answers` with probabilities, and the model writes no text.
Ollama serves this from version 0.40 onwards, for Laya. TypeSafe's hosted Jev
speaks the same API, which is what makes it a later provider, not a different
feature.

Ollama says which kind a model is. `GET /api/show` returns its capabilities:
`completion` for a text model, `decision` for Laya. This Mac's Ollama 0.40.0
currently lists these:

| Model                           | Capabilities                              | Kind     |
|---------------------------------|-------------------------------------------|----------|
| `gemma4:latest`, `gemma4:e4b`   | completion, vision, audio, tools, thinking | LLM      |
| `qwen3:4b-2507-64k`, `qwen3:4b-instruct-2507-q4_K_M` | completion, tools, thinking | LLM |
| `lfm2.5:latest`                 | completion, tools, thinking               | LLM      |
| `laya:latest`                   | decision                                  | System 1 |

## Choosing models

Outpost never keeps its own list of models. Each time the page loads, and
each time a setting is saved or the daemon starts, it asks Ollama:

1. `GET /api/tags` for the models that are pulled;
2. `GET /api/show` for each model's capabilities. These are cached by digest,
   so a model is described only once until it is re-pulled.

The LLM menu offers the models with `completion`, and the System 1 menu
offers those with `decision`. Each entry shows the size, the parameter count
and when it was last modified, so the operator can see what they're choosing.

Saving checks that the model is still in Ollama's list and has the right
capability. If the model has since been removed from Ollama, the page says so
and keeps the setting, without guessing a replacement. Requests then get a 503
naming the missing model until a new one is chosen or the model is pulled
again. When Ollama can't be reached, the page shows the current settings and
the error, and refuses to save, since nothing could be checked.

`config.toml` sets the same three values:

```toml
[ai]
ollama_url = "http://127.0.0.1:11434"   # the default
llm        = "gemma4:e4b"
system1    = "laya:latest"
```

The file can name a model Ollama doesn't have yet. The daemon logs a warning
on start and requests get a 503, the same as a model removed later. A file
that is ahead of `ollama pull` is a normal state, not an error that should
stop the daemon.

The portal's settings are kept in `ai.db`. Each value is overridden on its
own, and each has a "Use the config file's setting" button that removes the
override. Like `exposure.db`, `ai.db` is authoritative and backed up.

## What an app sees

Every app is started with:

```
OUTPOST_AI_URL=http://127.0.0.1:47901
OUTPOST_AI_TOKEN=oai_…            # this app's own
```

and calls the protocol it speaks, under a fixed prefix:

```
POST $OUTPOST_AI_URL/openai/v1/chat/completions   → the LLM
POST $OUTPOST_AI_URL/anthropic/v1/messages        → the LLM
POST $OUTPOST_AI_URL/systemone/v1/systemone       → the System 1 model
GET  $OUTPOST_AI_URL/openai/v1/models             → what "llm" and "system1" are now
```

The prefixes are there so off-the-shelf SDKs work unchanged. The OpenAI SDK
takes `base_url=$OUTPOST_AI_URL/openai/v1` and the Anthropic SDK takes
`base_url=$OUTPOST_AI_URL/anthropic`, each with the token as its API key.
Outpost accepts the token as `Authorization: Bearer` or `x-api-key`.

The path decides the model, not the request. Outpost replaces the body's
`model` with the configured one, so an app should send `"llm"` or `"system1"`
and never needs to know what is behind them. The reply's `model` field is left
as Ollama wrote it, so an app that logs it records what actually answered.
Streaming is passed through and flushed as it arrives.

| Situation                              | Response                                   |
|----------------------------------------|--------------------------------------------|
| No or unknown token                    | 401                                        |
| Path not in the list above             | 404                                        |
| That kind has no model configured      | 503 `no LLM is configured in Outpost`      |
| The configured model isn't in Ollama   | 503 naming the model                       |
| Ollama unreachable                     | 502                                        |
| Body over 32 MiB                       | 413                                        |

## Decisions

**One LLM and one System 1, Outpost-wide.** No per-app choice and no named
aliases. Apps written against "the LLM" don't need anything else, and two
settings fit on one page. If several LLMs are ever needed, `llm` becomes the
default name and the paths stay the same.

**Every app gets access.** Every app gets the variables, and there's no per-app
switch. Ollama already answers any local process without a key, so a per-app
allow list would protect nothing in phase 1. It belongs with the hosted
providers, where requests cost money.

**A proxy anyway, with a token.** Apps could be handed Ollama's address and a
model name instead, but going through Outpost has three advantages:

- **Switching is live.** The model is chosen per request, so no app restarts
  when it changes.
- **The contract is stable.** When hosted providers arrive, apps keep the
  same URL and token, and only what's behind them changes.
- **Usage is attributed.** The token names the app, so each request is logged
  and counted per app and per kind.

A token has to survive a daemon restart, because the apps outlive one. It is
derived from the app's name and a generation under a key in `ai.db`, so the
same one comes back each time without being stored. It changes only when an
admin regenerates it, which restarts that app.

**Its own loopback port, not the gateway.** The endpoint listens on
`127.0.0.1:47901` (`[ai] listen`) and only this machine can reach it. The
gateway serves people and strips identity, while this endpoint serves apps and
authenticates them by token. Keeping the two apart means no host the gateway
answers carries this path. It isn't on the tailnet or reachable through
Funnel.

**No translation.** Both chat APIs reach the LLM because Ollama speaks both
natively. Outpost only rewrites `model` and passes the body through.

**Logged and counted, not recorded.** Each request writes one daemon log line
with the app, the kind, the model, the status, the latency and token counts.
Prompts and answers are never logged. The page shows request counts per app
and kind since the daemon started, and the last error. Changes on the page go
to the activity feed.

**A test button for each kind.** For the LLM it sends a one-line prompt; for
System 1 it sends one `noul` question. It shows the reply and the latency.

## Not in phase 1

These are tracked in #120:

- hosted providers: Anthropic, OpenAI or Codex, and TypeSafe's Jev;
- the per-app access, keys and costs that come with hosted providers;
- more than one LLM or System 1 model;
- pulling models from the portal (`ollama pull` stays the operator's job;
  the page says when a configured model is missing);
- embeddings, images and audio;
- reaching the endpoint from off this machine.

## Plan

One PR, built in this order:

1. `internal/config`: the `[ai]` table (`ollama_url`, `llm`, `system1`,
   `listen`) and its validation.
2. `internal/ai`:
   - an Ollama client that lists models and capabilities, with a digest cache;
   - `ai.db`, holding the overrides, app token hashes and counters;
   - the effective settings;
   - the proxy, tested against a fake Ollama.
3. Supervisor: `OUTPOST_AI_URL` and `OUTPOST_AI_TOKEN` for every app.
4. Gateway: the admin-only **AI models** page, with the two menus from live
   Ollama, Test, usage, overrides and an activity event.
5. CLI:
   - `outpost ai` shows the settings and what Ollama has;
   - `outpost ai set llm <model>`;
   - `outpost ai test llm|system1`.
6. Docs:
   - the README section;
   - `outpost-service-spec.md`, for what an app gets and how to call it;
   - this note, updated to "built".

## Phase 2: hosted providers and app access

Built for [#120](https://github.com/sroberts/outpost/issues/120). Each kind
now comes from one provider:

| Provider | LLM, via | System 1, via | Models listed from |
|---|---|---|---|
| Ollama | `/v1/chat/completions`, `/v1/messages` | `/v1/systemone` | `/api/tags` and `/api/show` |
| OpenAI | `/v1/chat/completions` only | `/v1/decisions`, translated | `/v1/models`, chat models only; System 1 suggests `gpt-6-luna` |
| Anthropic | `/v1/messages`, `/v1/chat/completions` | — | `/v1/models` |
| OpenRouter | `/api/v1/chat/completions`, `/api/v1/messages` | `/api/v1/systemone` | `/api/v1/models`, and `?output_modalities=decisions` for System 1 |
| TypeSafe | — | `/v1/systemone` | none; suggests `jev-latest` |

The app-facing contract is unchanged: the same URL, token, paths and model
names. Decisions:

**One provider per kind, still one model per kind.** The choice became a
(provider, model) pair. `config.toml` gained `llm_provider` and
`system1_provider`, with Ollama the default, so a v0.5.0 file means what it
did. `ai.db`'s settings gained a `provider` column, added by a migration that
fills in `ollama` for what v0.5.0 saved.

**Keys are secrets, given to no app.** A provider's key is a secret named
`<provider>-api-key` (or `api_key_secret`), created unbound when it's set on
the page or with `outpost ai key`. That gives sealing, the Secrets page and
rotation for nothing. The secrets store gained `Value`, which the daemon alone
calls to add a key to a request; nothing on the socket or the portal can read
one. Rotating takes effect on the next request, and restarts no app, since no
app holds it.

**A mismatch is refused, not translated, for chat.** OpenAI is the one LLM
provider that doesn't take the Anthropic Messages API; Anthropic answers the
OpenAI API too, and Ollama and OpenRouter take both. So `/anthropic` with an
OpenAI LLM is a 400 naming the path to use, and the page and `outpost ai` say
so beside the choice. Translating chat formats, with tool calls and streaming
events, was judged not worth getting subtly wrong.

**System 1 to OpenAI is translated.** OpenAI's decision model takes the same
questions in its own Decisions API shape, so the proxy translates both ways:
questions keep the app's order (read with a token decoder, since a map loses
it), `noul` becomes `predicate`, criteria become `choices` and `levels`, a
JSON state becomes an `input_json` message, and the answers come back keyed
by name, with `score` levels keyed by index as System 1 has them. What
OpenAI can't take, such as a one-label choice or TypeSafe's `assets`, is
refused with a 400 saying so.

**Lists are complete or suggestions.** A provider whose list is complete has
a model checked against it when it's chosen. OpenAI's System 1 and TypeSafe
have no list, so their usual names are suggested and any ID is taken; Test is
how to check one. Hosted lists are cached for ten minutes (a minute after a
failure), since OpenRouter's is large and they change rarely. The page offers
them as a type-to-search list, since a radio per model doesn't scale to 380.

**Which apps may use each kind.** Hosted requests cost money, so each kind is
for every app (the default, and v0.5.0's behaviour) or a chosen list, set in
`config.toml` (`llm_apps`, `system1_apps`; unset is every app, `[]` none) or
on the page, which wins and is kept in `ai.db`'s new `access` table. A refused
app gets a 403 and isn't listed the model. Every app still gets the
variables, so giving one access later needs no restart.

**Compatibility kept, not rewritten.** v0.5.0's `Set`, `Options.LLM` and
`Overview.Models` keep their meaning, and the portal and the socket reach
the new operations through a separate `AIControls` interface, so phase 1's
callers and tests are unchanged.

**Not done in phase 2:** budgets, rate limits and per-app costs (token
counts are recorded, prices aren't applied); fallback from one provider to
another; more than one model per kind.
