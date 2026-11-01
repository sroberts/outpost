// Reload the catalog while an action runs, so the row shows when it's done.
// Not while a Manage menu or a confirmation is open: a reload would close it
// under the admin's thumb, nor while the filter holds something typed, which
// a reload would wipe. An open usage panel doesn't hold it back: it is only
// being read, and a row stuck on its action would be worse.
// Only loaded while something is under way.
(function () {
  "use strict";
  var filter = document.getElementById("filter");
  function tick() {
    if (document.querySelector("details[open]:not(.stats)") || (filter && filter.value)) {
      setTimeout(tick, 3000);
      return;
    }
    location.reload();
  }
  setTimeout(tick, 3000);
})();
