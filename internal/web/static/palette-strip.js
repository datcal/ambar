// Colour the sidebar's library-colour filter chips (M15).
//
// The chips carry their colour in data-hex rather than an inline style attribute, so
// the CSP stays `default-src 'self'` with no unsafe-inline (§11). This island does one
// thing: paint them. Without JavaScript the strip degrades to its percentages and the
// links still work, which is the information that matters most.
(function () {
    "use strict";

    // The contract: the element carries data-hex, its child swatch gets painted. The pack-strip
    // and swatch-find painters went with /palettes and the old palette panel in M16; the
    // sidebar's colour filter is what is left, and it is the one that gets used.
    var painters = [[".colour-chip[data-hex]", ".colour-swatch"]];
    painters.forEach(function (pair) {
        document.querySelectorAll(pair[0]).forEach(function (chip) {
            var swatch = chip.querySelector(pair[1]);
            if (swatch) {
                swatch.style.backgroundColor = chip.dataset.hex;
            }
        });
    });
})();
