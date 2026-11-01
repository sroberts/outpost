// Wait out Outpost's restart on the update page. While the daemon is down
// nothing answers, and a refresh would stop at the browser's error page, so
// this asks until the update is over and only then reloads.
(function () {
  "use strict";
  var every = 2000;
  var slowAfter = 10 * 60 * 1000;
  var began = Date.now();
  function later() {
    if (Date.now() - began > slowAfter) {
      var slow = document.getElementById("self-slow");
      if (slow) {
        slow.hidden = false;
      }
    }
    setTimeout(ask, every);
  }
  function ask() {
    fetch("/updates/self", { credentials: "same-origin", cache: "no-store" })
      .then(function (resp) {
        // Signed out, or no longer an admin: the page itself says so.
        if (resp.status === 401 || resp.status === 403) {
          location.reload();
          return null;
        }
        // A daemon is answering that has no such page: a release from
        // before this one, so the update is over and it can't say how.
        if (resp.status === 404) {
          location.reload();
          return null;
        }
        // Anything else that isn't an answer is Outpost still coming up.
        return resp.ok ? resp.json() : null;
      })
      .then(function (st) {
        if (st && st.final) {
          location.reload();
          return;
        }
        // The phase as it moves, from whichever daemon is answering.
        var phase = document.getElementById("self-phase");
        if (st && st.words && phase && phase.textContent !== st.words) {
          phase.textContent = st.words;
        }
        later();
      })
      .catch(later);
  }
  setTimeout(ask, every);
})();
