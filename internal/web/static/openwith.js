// Copy the local path (M14 "open in…").
//
// The path is already in the page as text — this only saves a select-and-copy. It
// prefers navigator.clipboard and falls back to a hidden textarea, because the
// clipboard API is unavailable on plain-HTTP LAN, which §8 calls out as a real
// deployment. A no-op on pages without the panel.
(function () {
    "use strict";

    var panel = document.getElementById("open-with");
    if (!panel) return;

    var button = panel.querySelector('[data-role="copy-path"]');
    var target = panel.querySelector('[data-role="path"]');
    if (!button || !target) return;

    function flash(text) {
        var previous = button.textContent;
        button.textContent = text;
        window.setTimeout(function () {
            button.textContent = previous;
        }, 1200);
    }

    function fallbackCopy(text) {
        var area = document.createElement("textarea");
        area.value = text;
        area.setAttribute("readonly", "readonly");
        area.className = "offscreen-copy";
        document.body.appendChild(area);
        area.select();
        var ok = false;
        try {
            ok = document.execCommand("copy");
        } catch (e) {
            ok = false;
        }
        document.body.removeChild(area);
        return ok;
    }

    button.addEventListener("click", function () {
        var text = target.textContent.trim();
        if (!text) return;

        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(
                function () { flash("copied"); },
                function () { flash(fallbackCopy(text) ? "copied" : "select it"); }
            );
            return;
        }
        flash(fallbackCopy(text) ? "copied" : "select it");
    });
})();
