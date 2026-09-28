// Solstein web UI: live updates (docs/style-guide.md, Live region).
//
// Progressive enhancement only. A page with a region marked data-live, while
// the server marks it "active" (work in progress), fetches the same page
// every few seconds and swaps in the new region, so everything outside it
// (a settings form being edited, the scroll position) is left alone. Forms
// inside the region marked data-live-submit post in the background the same
// way, instead of reloading the page. Without this script the page still
// works: it refreshes itself through its <noscript> fallback, and the forms
// post and come back to the same view and row.
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

  // A control in the region the user is on (a button, a link): the region
  // isn't swapped from under it.
  function usingControl() {
    var focused = document.activeElement;
    return !!focused && region.contains(focused) && focused.matches("a, button, input, select, textarea");
  }

  // swap puts in the region from a page Solstein sent, keeping focus on the
  // same row, and returns the page's notice, if it has one.
  function swap(html) {
    var page = new DOMParser().parseFromString(html, "text/html");
    var next = page.querySelector("[data-live]");
    if (!next) {
      throw new Error("no live region");
    }
    var focused = document.activeElement;
    var row = focused && region.contains(focused) && focused.closest ? focused.closest("tr[id]") : null;
    var before = summary();
    region.replaceWith(next);
    region = next;
    if (row) {
      var again = document.getElementById(row.id);
      if (again) {
        again.tabIndex = -1;
        again.focus({ preventScroll: true });
      }
    }
    var after = summary();
    if (after && after !== before) {
      announcer.textContent = after;
    }
    var notice = page.querySelector(".notice");
    return notice ? notice.textContent.trim() : "";
  }

  // fetchPage asks for a page, and fails as "signed-out" when the session
  // ended and Solstein sent the sign-in page instead.
  function fetchPage(url, options) {
    options = options || {};
    options.credentials = "same-origin";
    options.headers = { "Accept": "text/html" };
    return window.fetch(url, options).then(function (response) {
      if (response.redirected && new URL(response.url).pathname.indexOf("/ui/login") === 0) {
        throw new Error("signed-out");
      }
      if (!response.ok) {
        throw new Error("status " + response.status);
      }
      return response.text();
    });
  }

  function schedule() {
    window.clearTimeout(timer);
    timer = null;
    if (isActive() && !paused) {
      timer = window.setTimeout(update, interval);
    }
  }

  function update() {
    // Not while someone is using a control in the region, nor while nobody
    // looks.
    if (document.hidden || usingControl()) {
      schedule();
      return;
    }
    fetchPage(region.getAttribute("data-live-url")).then(function (html) {
      swap(html);
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

  // Actions in the region post in the background: the answer is the page
  // they redirect to, whose region is swapped in, so nothing reloads and the
  // scroll position stays where it was.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!form.matches("form[data-live-submit]") || !region.contains(form)) {
      return;
    }
    event.preventDefault();
    var submit = form.querySelector("button");
    var row = form.closest("tr[id]");
    if (submit) {
      submit.disabled = true;
    }
    if (row) {
      // Keep the place: the button goes once the episode is queued.
      row.tabIndex = -1;
      row.focus({ preventScroll: true });
    }
    fetchPage(form.action, { method: "POST", body: new URLSearchParams(new FormData(form)) }).then(function (html) {
      var notice = swap(html);
      describe();
      if (notice) {
        text.textContent = notice + " ";
        announcer.textContent = notice;
      }
      schedule();
    }).catch(function (error) {
      if (error.message === "signed-out") {
        show("Signed out; reload the page to sign in again.", "");
        return;
      }
      // Let the browser post it the ordinary way, and show what happened.
      form.submit();
    });
  });

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
