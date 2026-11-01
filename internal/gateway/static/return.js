// Bring the catalog up to date when its tab comes back into view, so a crash
// that happened meanwhile doesn't sit behind an old "running". There's no timer
// here on purpose: every load renews the session, so a page refreshing itself
// would keep a forgotten tab signed in for good. A glance away isn't worth a
// reload, and an open menu or a typed filter isn't worth losing for one.
(function () {
  "use strict";
  var filter = document.getElementById("filter");
  var hiddenAt = document.hidden ? Date.now() : 0;
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      hiddenAt = Date.now();
      return;
    }
    var away = Date.now() - hiddenAt;
    if (away < 15000 || document.querySelector("details[open]") || (filter && filter.value)) return;
    location.reload();
  });
})();
