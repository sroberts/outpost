// Follow new lines on the Logs page: while the box is ticked and the tab is
// in view, fetch this same view again every few seconds and put its lines in
// place of these, keeping a reader who was at the bottom there. Without the
// script the box stays hidden, and Refresh does the same by hand. Not while
// the tab is out of view: every load renews the session, and a forgotten tab
// would keep it alive.
(function () {
  "use strict";
  var box = document.getElementById("log-follow");
  if (!box || !document.getElementById("log-body")) {
    return;
  }
  box.parentElement.hidden = false;
  var every = 5000;
  var timer = null;

  function atBottom() {
    return window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 80;
  }
  function swap(doc, sel) {
    var mine = document.querySelector(sel);
    var theirs = doc.querySelector(sel);
    if (mine && theirs) {
      mine.replaceChildren.apply(mine, Array.prototype.slice.call(theirs.childNodes));
    }
  }
  function schedule() {
    if (box.checked && timer === null) {
      timer = setTimeout(tick, every);
    }
  }
  function tick() {
    timer = null;
    if (!box.checked) {
      return;
    }
    if (document.hidden) {
      schedule();
      return;
    }
    var stick = atBottom();
    fetch(location.pathname + location.search, { credentials: "same-origin" })
      .then(function (resp) {
        // Signed out, or the daemon is restarting: leave the lines as
        // they are and try again later.
        if (!resp.ok || resp.redirected) {
          throw new Error("not the page");
        }
        return resp.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        if (!doc.getElementById("log-body")) {
          return;
        }
        swap(doc, "#log-body");
        swap(doc, ".log-count");
        if (stick) {
          window.scrollTo(0, document.documentElement.scrollHeight);
        }
      })
      .catch(function () {})
      .then(schedule);
  }

  box.addEventListener("change", function () {
    // Kept in the address, so a reload or a shared link follows too.
    var u = new URL(location.href);
    if (box.checked) {
      u.searchParams.set("follow", "1");
    } else {
      u.searchParams.delete("follow");
    }
    history.replaceState(null, "", u.pathname + u.search + u.hash);
    schedule();
  });
  schedule();
})();
