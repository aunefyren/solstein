// Solstein web UI: live updates (docs/style-guide.md, Live region).
//
// Progressive enhancement only. A page with a region marked data-live, while
// the server marks it "active" (work in progress), fetches the same page
// every few seconds and swaps in the new region, so everything outside it
// (a settings form being edited, the scroll position) is left alone.
// Without this script the page still works, and refreshes itself through its
// <noscript> fallback instead.
(function () {
  "use strict";

  var region = document.querySelector("[data-live]");
  var status = document.querySelector("[data-live-status]");
  if (!region || !status || !window.fetch || !window.DOMParser) {
    return;
  }

  var pauseKey = "solstein-live-paused";
  var interval = parseInt(region.getAttribute("data-live-interval"), 10) || 5000;
  var timer = null;
  var paused = false;
  try {
    paused = window.sessionStorage.getItem(pauseKey) === "1";
  } catch (error) {
    // Storage can be off; pausing then lasts until the page is left.
  }

  // The status line: a sentence, and a Pause or Resume button.
  var text = document.createElement("span");
  var button = document.createElement("button");
  button.type = "button";
  button.className = "button button--secondary";
  // Announced to screen readers only when the region's summary changes.
  var announcer = document.createElement("span");
  announcer.className = "visually-hidden";
  announcer.setAttribute("aria-live", "polite");
  status.textContent = "";
  status.appendChild(text);
  status.appendChild(button);
  status.appendChild(announcer);

  function isActive() {
    return region.getAttribute("data-live") === "active";
  }

  function summary() {
    var element = region.querySelector("[data-live-summary]");
    return element ? element.textContent : "";
  }

  function show(sentence, buttonLabel) {
    text.textContent = sentence + (buttonLabel ? " " : "");
    button.textContent = buttonLabel || "";
    button.hidden = !buttonLabel;
    status.hidden = false;
  }

  function describe() {
    if (!isActive()) {
      show("Up to date.", "");
    } else if (paused) {
      show("Updates paused.", "Resume");
    } else {
      show("Updating every " + Math.round(interval / 1000) + " s while " + region.getAttribute("data-live-note") + ".", "Pause");
    }
  }

  function schedule() {
    window.clearTimeout(timer);
    timer = null;
    if (isActive() && !paused) {
      timer = window.setTimeout(update, interval);
    }
  }

  function update() {
    // Not while someone is using the region, nor while nobody looks.
    var focused = document.activeElement;
    if (document.hidden || (focused && focused !== document.body && region.contains(focused))) {
      schedule();
      return;
    }
    var before = summary();
    window.fetch(region.getAttribute("data-live-url"), {
      credentials: "same-origin",
      headers: { "Accept": "text/html" }
    }).then(function (response) {
      if (response.redirected && new URL(response.url).pathname.indexOf("/ui/login") === 0) {
        throw new Error("signed-out");
      }
      if (!response.ok) {
        throw new Error("status " + response.status);
      }
      return response.text();
    }).then(function (html) {
      var next = new DOMParser().parseFromString(html, "text/html").querySelector("[data-live]");
      if (!next) {
        throw new Error("no live region");
      }
      region.replaceWith(next);
      region = next;
      var after = summary();
      if (after && after !== before) {
        announcer.textContent = after;
      }
      describe();
      schedule();
    }).catch(function (error) {
      if (error.message === "signed-out") {
        show("Signed out; reload the page to sign in again.", "");
        return;
      }
      show("Couldn't update just now; trying again.", "Pause");
      schedule();
    });
  }

  button.addEventListener("click", function () {
    paused = !paused;
    try {
      window.sessionStorage.setItem(pauseKey, paused ? "1" : "0");
    } catch (error) {
      // As above: in memory only.
    }
    describe();
    if (paused) {
      window.clearTimeout(timer);
    } else {
      update();
    }
  });

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && isActive() && !paused) {
      update();
    }
  });

  if (isActive()) {
    describe();
    schedule();
  }
})();
