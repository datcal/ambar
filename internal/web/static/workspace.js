// The workspace shell (M14): thumbnail size and the folder panel's width, remembered.
//
// Two rules this island follows:
//
//   - It writes CSS custom properties through the CSSOM, never an inline style
//     attribute, so the CSP stays `default-src 'self'` with no unsafe-inline (§11).
//   - It degrades to nothing. Without JavaScript the grid renders at the default
//     tile size and panel width from app.css and every link still works; only the slider
//     and the panel's resize handle go inert.
(function () {
    "use strict";

    var KEY = "ambar.tile";
    var root = document.documentElement;

    function apply(rem) {
        root.style.setProperty("--tile", rem + "rem");
    }

    // --- the folder panel's width -------------------------------------------------
    //
    // Long pack names and folders four levels down do not fit a fixed column, so the panel
    // can be dragged wider (or narrower) from the line beside it, or set from the keyboard
    // with the arrow keys on that line. Double-click puts it back. Remembered per browser,
    // like the tile size; without JavaScript the panel keeps the width app.css gives it.
    var PANEL_KEY = "ambar.panelLeft";
    var PANEL_MIN = 12;
    var PANEL_MAX = 40;
    var handle = document.querySelector('[data-role="panel-resize"]');
    var panel = document.getElementById("panel-left");

    function remPx() {
        return parseFloat(window.getComputedStyle(root).fontSize) || 16;
    }

    function setPanel(rem, save) {
        rem = Math.round(Math.max(PANEL_MIN, Math.min(PANEL_MAX, rem)) * 4) / 4;
        root.style.setProperty("--panel-left", rem + "rem");
        if (handle) handle.setAttribute("aria-valuenow", String(rem));
        if (!save) return;
        try {
            window.localStorage.setItem(PANEL_KEY, String(rem));
        } catch (e) {
            // Not fatal: the width still applies for this page view.
        }
    }

    function panelRem() {
        return panel ? panel.getBoundingClientRect().width / remPx() : 18;
    }

    var storedPanel = null;
    try {
        storedPanel = window.localStorage.getItem(PANEL_KEY);
    } catch (e) {
        storedPanel = null;
    }
    if (storedPanel && parseFloat(storedPanel) >= PANEL_MIN) setPanel(parseFloat(storedPanel), false);

    if (handle && panel) {
        handle.setAttribute("aria-valuenow", String(Math.round(panelRem())));

        handle.addEventListener("pointerdown", function (event) {
            if (event.button !== 0) return;
            event.preventDefault();
            handle.setPointerCapture(event.pointerId);
            document.body.classList.add("resizing");
            var left = panel.getBoundingClientRect().left;

            function move(e) {
                setPanel((e.clientX - left) / remPx(), false);
            }
            function stop(e) {
                handle.removeEventListener("pointermove", move);
                handle.removeEventListener("pointerup", stop);
                handle.removeEventListener("pointercancel", stop);
                document.body.classList.remove("resizing");
                setPanel((e.clientX - left) / remPx(), true);
            }
            handle.addEventListener("pointermove", move);
            handle.addEventListener("pointerup", stop);
            handle.addEventListener("pointercancel", stop);
        });

        handle.addEventListener("keydown", function (event) {
            var step = event.shiftKey ? 4 : 1;
            var now = panelRem();
            switch (event.key) {
                case "ArrowRight": setPanel(now + step, true); break;
                case "ArrowLeft": setPanel(now - step, true); break;
                case "Home": setPanel(PANEL_MIN, true); break;
                case "End": setPanel(PANEL_MAX, true); break;
                default: return;
            }
            event.preventDefault();
        });

        handle.addEventListener("dblclick", function () {
            root.style.removeProperty("--panel-left");
            try {
                window.localStorage.removeItem(PANEL_KEY);
            } catch (e) {
                // Nothing stored.
            }
            handle.setAttribute("aria-valuenow", String(Math.round(panelRem())));
        });
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
