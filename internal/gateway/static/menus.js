// Plain details elements work without any of this. With it, a confirmation
// gets a real Cancel, Escape closes the innermost menu showing and hands focus
// back to what opened it, and a reload lands on the note it was sent to.
(function () {
  "use strict";
  function close(d) {
    d.open = false;
    d.querySelector("summary").focus();
  }
  // One Cancel a confirmation, however many forms it holds: after the last
  // of its own forms, beside what it submits, or at its foot when its forms
  // are all folded inside a choice of their own.
  document.querySelectorAll("details.confirm").forEach(function (d) {
    var forms = Array.prototype.filter.call(d.querySelectorAll("form"), function (f) {
      return f.closest("details") === d;
    });
    if (!forms.length && !d.querySelector("form")) return;
    var cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "plain cancel";
    cancel.textContent = "Cancel";
    cancel.addEventListener("click", function () { close(d); });
    if (forms.length) forms[forms.length - 1].after(cancel);
    else d.append(cancel);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    // A menu closed by opening another keeps its own confirmation open, unseen.
    var open = Array.prototype.filter.call(
      document.querySelectorAll("details.confirm[open], details.acts[open]"),
      function (d) { return d.offsetParent !== null; }
    );
    if (open.length) close(open[open.length - 1]);
  });
  // A link to something folded away, such as a provider's key on the AI
  // page, opens what it's folded in; not every browser does that itself.
  function reveal() {
    var t = location.hash && document.getElementById(location.hash.slice(1));
    if (!t) return null;
    var folded = false;
    for (var d = t; d; d = d.parentElement) {
      if (d.tagName === "DETAILS" && !d.open) { d.open = true; folded = true; }
    }
    if (folded) t.scrollIntoView();
    // A link straight to a form, such as a provider's key, is a link to
    // fill it in.
    var field = t.tagName === "DETAILS" && t.querySelector("input:not([type=hidden])");
    if (field) field.focus({ preventScroll: true });
    return t;
  }
  window.addEventListener("hashchange", reveal);
  // Following a link to the hash already in the address fires no
  // hashchange, so a form closed since would stay folded.
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest("a[href^='#']");
    if (a && a.getAttribute("href") === location.hash) reveal();
  });
  var target = reveal();
  if (target && target.hasAttribute("tabindex")) target.focus({ preventScroll: true });
})();
