// Notifications for this device. On the account page it turns them on and
// off; on every signed-in page it keeps the server's copy of this device's
// subscription current, since a browser can replace a subscription without
// saying so, and a new sign-in on the same device needs it handed over again.
(function () {
  "use strict";
  var api = "/.outpost/push/subscription";
  var seen = "outpost-push-endpoint"; // the endpoint last handed over
  var key = document.body.getAttribute("data-push-key");
  var section = document.getElementById("notify");
  if (!key) return;

  var ios = /iP(hone|ad|od)/.test(navigator.userAgent) ||
    (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  var standalone = navigator.standalone === true ||
    window.matchMedia("(display-mode: standalone)").matches;
  // Declarative Web Push (Safari 18.4 and on) needs no service worker.
  var declarative = "pushManager" in window;
  var classic = "serviceWorker" in navigator && "PushManager" in window;
  var supported = (declarative || classic) && "Notification" in window;

  function keyBytes(s) {
    var b = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
    var out = new Uint8Array(b.length);
    for (var i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
    return out;
  }

  function sameKey(sub) {
    var k = sub.options && sub.options.applicationServerKey;
    if (!k) return true; // can't tell; assume it is
    var a = new Uint8Array(k), b = keyBytes(key);
    if (a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
    return true;
  }

  function manager() {
    if (declarative) return Promise.resolve(window.pushManager);
    return navigator.serviceWorker.register("/sw.js", { scope: "/" })
      .then(function () { return navigator.serviceWorker.ready; })
      .then(function (reg) { return reg.pushManager; });
  }

  function send(method, body) {
    return fetch(api, {
      method: method,
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (r.ok) return;
      return r.json().then(
        function (e) { throw new Error(e.error || "Outpost answered " + r.status); },
        function () { throw new Error("Outpost answered " + r.status); });
    });
  }

  function handOver(sub) {
    var j = sub.toJSON();
    var old = localStorage.getItem(seen);
    return send("POST", {
      endpoint: j.endpoint,
      keys: j.keys,
      old_endpoint: old && old !== j.endpoint ? old : ""
    }).then(function () {
      localStorage.setItem(seen, j.endpoint);
      return sub;
    });
  }

  function subscribe(m) {
    return m.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(key) }).then(handOver);
  }

  // current is this device's subscription, brought up to date: made again
  // if it vanished or was made with another key, and handed over if the
  // server may not have it. Only once permission is granted: before that,
  // asking would raise the browser's prompt without a tap.
  function current() {
    if (!supported || Notification.permission !== "granted" || (ios && !standalone)) {
      return Promise.resolve(null);
    }
    return manager().then(function (m) {
      return m.getSubscription().then(function (sub) {
        if (sub && !sameKey(sub)) {
          return sub.unsubscribe().then(function () { return subscribe(m); });
        }
        if (!sub) return localStorage.getItem(seen) ? subscribe(m) : null;
        var here = section ? section.getAttribute("data-here") === "1" : true;
        if (localStorage.getItem(seen) === sub.endpoint && here) return sub;
        return handOver(sub);
      });
    });
  }

  if (!section) {
    current().catch(function () { /* the account page says what's wrong */ });
    return;
  }

  function show(state, message) {
    var parts = section.querySelectorAll("[data-show]");
    for (var i = 0; i < parts.length; i++) {
      parts[i].hidden = parts[i].getAttribute("data-show") !== state &&
        !(state === "error-off" && parts[i].getAttribute("data-show") === "off");
    }
    if (message) {
      var e = section.querySelector('[data-show="error"]');
      e.textContent = message;
      e.hidden = false;
    }
  }

  function settle() {
    if (!supported) return show(ios && !standalone ? "install" : "unsupported");
    if (ios && !standalone) return show("install");
    if (Notification.permission === "denied") return show("denied");
    return current().then(function (sub) { show(sub ? "on" : "off"); });
  }

  function failed(err) {
    show("error-off", "Notifications weren't turned on: " + (err && err.message ? err.message : err));
  }

  section.addEventListener("click", function (ev) {
    var b = ev.target.closest("[data-push]");
    if (!b) return;
    b.disabled = true;
    var done = function () { b.disabled = false; };
    if (b.getAttribute("data-push") === "on") {
      // Asked here, in the tap: Safari refuses a prompt that isn't.
      Notification.requestPermission().then(function (p) {
        if (p !== "granted") return show(p === "denied" ? "denied" : "off");
        return manager().then(subscribe).then(function () {
          window.location.reload(); // the device list and test button follow
        });
      }).catch(failed).then(done);
      return;
    }
    manager().then(function (m) { return m.getSubscription(); }).then(function (sub) {
      if (!sub) return;
      var endpoint = sub.endpoint;
      return sub.unsubscribe().then(function () { return send("DELETE", { endpoint: endpoint }); });
    }).then(function () {
      localStorage.removeItem(seen);
      window.location.reload();
    }).catch(function (err) {
      show("error-off", "Notifications weren't turned off: " + (err && err.message ? err.message : err));
    }).then(done);
  });

  settle().catch(failed);
})();
