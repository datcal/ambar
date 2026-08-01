// Keyboard browsing on the asset page (§8's "full keyboard navigation", M16).
//
// j/k and the arrow keys step through the browse order; the links they follow are the ones
// already rendered in the rail, so the server decides what "next" means and this island
// only presses the button. Nothing happens on a page without those links, so loading it
// everywhere costs one cached request.
//
// Two rules keep it out of the way:
//
//   - Never while typing. A tag input, the search box or any editable element owns the
//     keyboard while it has focus, and stealing `j` from someone typing "jungle" would be
//     worse than having no shortcut at all.
//   - Never inside the viewer. viewer2d.js binds the arrow keys to panning on the focused
//     stage, so an event that came from there is already spoken for.
(function () {
    "use strict";

    function isTyping(el) {
        if (!el) return false;
        if (el.isContentEditable) return true;
        const tag = el.tagName;
        return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
    }

    document.addEventListener("keydown", function (event) {
        if (event.ctrlKey || event.metaKey || event.altKey) return;
        if (isTyping(event.target)) return;
        // The 2D and 3D stages handle their own arrows (pan, orbit).
        if (event.target instanceof Element && event.target.closest(".viewer, .model")) return;

        let role = null;
        switch (event.key) {
            case "j":
            case "ArrowRight":
                role = "next-asset";
                break;
            case "k":
            case "ArrowLeft":
                role = "prev-asset";
                break;
            default:
                return;
        }

        const link = document.querySelector('[data-role="' + role + '"]');
        if (!link) return; // an end of the list, or not an asset page
        event.preventDefault();
        window.location.href = link.href;
    });
})();
