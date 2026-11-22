// Spec 021: only this script sends the fragment, so a client that fetches /r without running it changes nothing.
(function () {
  var status = document.getElementById("status");

  function fail(message) {
    var inbox = document.createElement("a");
    inbox.href = "/inbox";
    inbox.textContent = "Open the inbox";
    status.textContent = message + " ";
    status.appendChild(inbox);
  }

  var link = location.hash.replace(/^#/, "");
  if (!link) {
    fail("This link is not valid.");
    return;
  }
  fetch("/api/v1/open", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ link: link }),
  })
    .then(function (res) {
      if (res.status === 429 || res.status >= 500) {
        throw new Error("HTTP " + res.status);
      }
      // Any other refusal (404, 400, 413) is a link that can never work: retrying would not help.
      if (!res.ok) {
        fail("This link is not valid.");
        return;
      }
      return res.json().then(function (body) {
        location.replace(body.url);
      });
    })
    .catch(function () {
      fail("gedour could not be reached, try again.");
    });
})();
