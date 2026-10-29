// The catalog's filter: hide the stations that don't match what's typed.
// The page works without it; it's only loaded once the list is long.
(function () {
  "use strict";
  var input = document.getElementById("filter");
  var line = document.getElementById("line");
  var none = document.getElementById("nomatch");
  if (!input || !line) return;
  var stops = Array.prototype.slice.call(line.querySelectorAll("li[data-find]"));
  input.hidden = false;
  input.closest("label").hidden = false;
  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    var shown = 0;
    stops.forEach(function (li) {
      var hit = q === "" || li.getAttribute("data-find").indexOf(q) !== -1;
      li.hidden = !hit;
      if (hit) shown++;
    });
    if (none) none.hidden = shown !== 0;
  });
})();
