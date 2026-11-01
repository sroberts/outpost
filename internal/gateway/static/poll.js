// Reload the catalog while an action runs or an app is coming back, so the row
// shows when it's done. Not while a Manage menu or a confirmation is open: a
// reload would close it under the admin's thumb, nor while the filter holds
// something typed, which a reload would wipe. An open usage panel doesn't hold
// back an action, which it could leave stuck on screen, but it does hold back
// an app crash-looping, which is just when someone wants to read its graphs.
// Only loaded while something is under way.
(function () {
  "use strict";
  var filter = document.getElementById("filter");
  function tick() {
    var acting = document.querySelector(".stop.busy");
    if (document.querySelector(acting ? "details[open]:not(.stats)" : "details[open]") || (filter && filter.value)) {
      setTimeout(tick, 3000);
      return;
    }
    location.reload();
  }
  setTimeout(tick, 3000);
})();
