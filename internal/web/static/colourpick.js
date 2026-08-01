// The sidebar's colour picker (M17), and the one part of it the browser will not do in
// plain HTML.
//
// Everything else here is a GET form: <input type="color"> gives the native picker, the
// text field takes #aabbcc or rgb(1,2,3), and the server composes the query. This island
// adds only the eyedropper — "sample the colour of that thing on my screen" — which is the
// EyeDropper API, and which Chromium has and Firefox does not.
//
// So the button is created here rather than sitting in the template: a control that is
// present and does nothing is worse than one that is absent, and there is no polyfill for
// reading a pixel outside the page.

(function () {
    "use strict";

    var form = document.querySelector(".colour-pick");
    if (!form) return;

    var swatch = form.querySelector('input[type="color"]');
    var typed = form.querySelector('input[name="typed"]');
    if (!swatch || !typed) return;

    // Picking from the wheel clears a stale typed value, because the server prefers the
    // typed one and a leftover from a minute ago would silently win.
    swatch.addEventListener("input", function () { typed.value = ""; });

    if (typeof window.EyeDropper !== "function") return;

    var button = document.createElement("button");
    button.type = "button";
    button.className = "swatch-icon colour-dropper";
    button.textContent = "⊙";
    button.title = "Pick a colour from anywhere on screen";
    button.setAttribute("aria-label", "Pick a colour from anywhere on screen");

    button.addEventListener("click", function () {
        new window.EyeDropper()
            .open()
            .then(function (result) {
                // Into the text field, not the wheel: it is the input the server reads
                // first, and seeing the hex is half the point of sampling one.
                typed.value = result.sRGBHex;
            })
            .catch(function () {
                // Escape closes the dropper and rejects. Nothing to report.
            });
    });

    form.insertBefore(button, form.querySelector('button[type="submit"]'));
})();
