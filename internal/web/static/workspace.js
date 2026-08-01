// The workspace shell (M14): thumbnail size, remembered.
//
// Two rules this island follows:
//
//   - It writes CSS custom properties through the CSSOM, never an inline style
//     attribute, so the CSP stays `default-src 'self'` with no unsafe-inline (§11).
//   - It degrades to nothing. Without JavaScript the grid renders at the default
//     tile size from app.css and every link still works; only the slider goes inert.
(function () {
    "use strict";

    var KEY = "ambar.tile";
    var root = document.documentElement;

    function apply(rem) {
        root.style.setProperty("--tile", rem + "rem");
    }

    var slider = document.getElementById("tilesize");

    // Restore before first paint of the grid where possible: the script is deferred,
    // so this runs after parsing but before images have laid out.
    var stored = null;
    try {
        stored = window.localStorage.getItem(KEY);
    } catch (e) {
        // Private mode or a locked-down browser. The default size is fine.
        stored = null;
    }
    if (stored) {
        var n = parseFloat(stored);
        if (n >= 4 && n <= 26) {
            apply(n);
            if (slider) slider.value = String(n);
        }
    }

    if (!slider) return;

    slider.addEventListener("input", function () {
        var rem = parseFloat(slider.value);
        if (!(rem >= 4 && rem <= 26)) return;
        apply(rem);
        try {
            window.localStorage.setItem(KEY, String(rem));
        } catch (e) {
            // Not fatal: the size still applies for this page view.
        }
    });

    // There used to be a Ctrl/⌘ +/- shortcut here that resized the tiles.
    // Removed in M16: it took over the browser's own zoom on the busiest page in the
    // application, it was documented nowhere but in this comment, and the slider it
    // duplicated is two centimetres away.
})();
