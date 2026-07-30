// The live font specimen (M15).
//
// Registers the asset's own face with the FontFace API and applies it to a ladder of
// sizes, so "does my UI label look right in this?" is answerable by typing rather
// than by imagining. Nothing is uploaded and nothing is written: the text lives in
// the input.
//
// Font family names are generated here rather than taken from the file, so a family
// called `sans-serif` or one containing a quote cannot escape into the CSS. The face
// is applied through the CSSOM (`element.style.fontFamily`), never an inline style
// attribute in the HTML, so the CSP stays `default-src 'self'` (§11).
(function () {
    "use strict";

    var panel = document.getElementById("specimen");
    if (!panel) return;

    var status = panel.querySelector('[data-role="status"]');
    var input = panel.querySelector('[data-role="text"]');
    var lines = Array.prototype.slice.call(panel.querySelectorAll('[data-role="line"]'));
    if (!lines.length) return;

    // A name of our own making: unique per page, and impossible to confuse with a
    // generic family.
    var family = "ambar-specimen-" + Math.floor(Math.random() * 1e9).toString(36);

    function paint() {
        var text = input && input.value !== "" ? input.value : "The quick brown fox";
        lines.forEach(function (line) {
            var size = parseInt(line.dataset.size, 10) || 16;
            line.textContent = text;
            line.style.fontFamily = family + ", system-ui, sans-serif";
            line.style.fontSize = size + "px";
            line.setAttribute("title", size + "px");
        });
    }

    function fail(message) {
        if (status) {
            status.textContent = message;
            status.classList.add("badge-warn");
        }
        // Still show the text at each size in the page font: the sizes are useful even
        // when the face cannot be loaded, and an empty panel explains nothing.
        paint();
    }

    if (typeof window.FontFace === "undefined" || !document.fonts) {
        fail("this browser cannot preview fonts");
        return;
    }

    var face = new FontFace(family, 'url("' + panel.dataset.src + '")');
    face.load().then(
        function (loaded) {
            document.fonts.add(loaded);
            if (status) {
                status.textContent = panel.dataset.family || "loaded";
                status.classList.remove("badge-warn");
            }
            paint();
        },
        function () {
            // A .woff2, a broken file, or a format the browser will not accept.
            fail("this format cannot be previewed in a browser");
        }
    );

    if (input) input.addEventListener("input", paint);
    paint();
})();
