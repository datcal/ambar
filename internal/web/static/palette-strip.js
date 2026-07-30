// Colour the pack-palette strips (§7 pack palette consistency view).
//
// The chips carry their colour in data-hex rather than an inline style attribute, so
// the CSP stays `default-src 'self'` with no unsafe-inline (§11). This island does one
// thing: paint them. Without JavaScript the strip degrades to its percentages and the
// links still work, which is the information that matters most.
(function () {
    "use strict";

    // Pack palette strips (§7) and the sidebar's library-colour filter (M15) use the
    // same contract: the element carries data-hex, its child swatch gets painted.
    var painters = [
        [".strip-chip[data-hex]", ".strip-swatch"],
        [".colour-chip[data-hex]", ".colour-swatch"],
        [".swatch-find[data-hex]", ".find-swatch"],
    ];
    painters.forEach(function (pair) {
        document.querySelectorAll(pair[0]).forEach(function (chip) {
            var swatch = chip.querySelector(pair[1]);
            if (swatch) {
                swatch.style.backgroundColor = chip.dataset.hex;
            }
        });
    });
})();
