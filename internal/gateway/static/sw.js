// The portal's service worker, for browsers without Declarative Web Push:
// Chromium, Firefox, and Safari before 18.4. Every notification Outpost sends
// is declarative JSON, which Safari 18.4 and on shows by itself; here the
// same JSON is shown by hand, so it looks the same everywhere.
"use strict";

self.addEventListener("install", function () { self.skipWaiting(); });
self.addEventListener("activate", function (ev) { ev.waitUntil(self.clients.claim()); });

self.addEventListener("push", function (ev) {
  var msg = {};
  try { msg = ev.data ? ev.data.json() : {}; } catch (e) { msg = {}; }
  var n = msg.notification || {};
  var title = n.title || "Outpost";
  var opts = {
    body: n.body || "",
    tag: n.tag || undefined,
    lang: n.lang || undefined,
    dir: n.dir || "auto",
    silent: !!n.silent,
    icon: "/.outpost/static/icon-180.png",
    data: { navigate: n.navigate || "/", data: n.data || null }
  };
  var shown = self.registration.showNotification(title, opts);
  if (msg.app_badge !== undefined && "setAppBadge" in self.navigator) {
    var count = parseInt(msg.app_badge, 10) || 0;
    shown = shown.then(function () {
      return count > 0 ? self.navigator.setAppBadge(count) : self.navigator.clearAppBadge();
    }).catch(function () { /* a badge is a nicety */ });
  }
  ev.waitUntil(shown);
});

self.addEventListener("notificationclick", function (ev) {
  ev.notification.close();
  var target = new URL((ev.notification.data && ev.notification.data.navigate) || "/", self.location.origin);
  // Only ever this origin: the portal sends a device on to an app itself.
  if (target.origin !== self.location.origin) target = new URL("/", self.location.origin);
  ev.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (list) {
    for (var i = 0; i < list.length; i++) {
      if (list[i].url === target.href && "focus" in list[i]) return list[i].focus();
    }
    return self.clients.openWindow(target.href);
  }));
});

// The push service replaced the subscription: make a new one with the same
// key and hand it over, so notifications carry on without a visit.
self.addEventListener("pushsubscriptionchange", function (ev) {
  var old = ev.oldSubscription;
  var key = old && old.options ? old.options.applicationServerKey : null;
  if (!key) return;
  ev.waitUntil(self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
    .then(function (sub) {
      var j = sub.toJSON();
      return fetch("/.outpost/push/subscription", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ endpoint: j.endpoint, keys: j.keys, old_endpoint: old.endpoint })
      });
    }));
});
