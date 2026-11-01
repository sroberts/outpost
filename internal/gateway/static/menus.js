// Plain details elements work without any of this. With it, a confirmation
// gets a real Cancel, Escape closes the innermost menu showing and hands focus
// back to what opened it, and a reload lands on the note it was sent to.
(function () {
  "use strict";
  function close(d) {
    d.open = false;
    d.querySelector("summary").focus();
  }
  document.querySelectorAll("details.confirm > form").forEach(function (form) {
    var d = form.parentElement;
    var cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "plain cancel";
    cancel.textContent = "Cancel";
    cancel.addEventListener("click", function () { close(d); });
    form.after(cancel);
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
  var target = location.hash && document.getElementById(location.hash.slice(1));
  if (target && target.hasAttribute("tabindex")) target.focus({ preventScroll: true });
})();
