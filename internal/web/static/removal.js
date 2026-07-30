// Select-all for one removal finding (§9.1, M13).
//
// §9.1 permits a "select all in this finding" control — "choosing two hundred
// things one at a time is its own kind of hazard" — as long as it stays a
// deliberate act. So this island does exactly one thing: it makes the header
// checkbox mirror its own table's checkboxes. It never ticks anything on load, it
// never submits a form, and it never crosses from one finding's table into
// another's.
//
// Without JavaScript the page still works: the header checkbox is inert and the
// per-row checkboxes are ordinary form controls.
//
// An external module rather than an inline handler, so the CSP stays
// `default-src 'self'` with no unsafe-inline (§11).
(function () {
    "use strict";

    function rowBoxes(scope) {
        return Array.from(scope.querySelectorAll('tbody input[type="checkbox"][name="path"]'));
    }

    document.querySelectorAll("[data-select-all]").forEach(function (master) {
        var scope = master.closest("[data-select-all-scope]");
        if (!scope) {
            return;
        }

        master.addEventListener("change", function () {
            rowBoxes(scope).forEach(function (box) {
                box.checked = master.checked;
            });
        });

        // Keep the header honest when rows are ticked individually: a half-selected
        // finding must not look fully selected.
        scope.addEventListener("change", function (event) {
            var target = event.target;
            if (!(target instanceof HTMLInputElement) || target.name !== "path") {
                return;
            }
            var boxes = rowBoxes(scope);
            var checked = boxes.filter(function (box) {
                return box.checked;
            }).length;
            master.checked = checked === boxes.length && boxes.length > 0;
            master.indeterminate = checked > 0 && checked < boxes.length;
        });
    });
})();
