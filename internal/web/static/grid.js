// The grid's interactions (M16): hover animation, keyboard navigation, selection that
// survives paging, and the two dropdowns that submit themselves.
//
// A JS island like the rest: no framework, no bundler, everything from data- attributes so
// the CSP stays `default-src 'self'`. Without JavaScript the grid still lists, links, pages
// and sorts — the selects grow a submit button (<noscript>), tiles stay static images, and
// selection works within one page through ordinary checkboxes.
(function () {
    "use strict";

    const grid = document.getElementById("thumbgrid");
    const form = document.querySelector(".bulkform");

    // --- self-submitting dropdowns ------------------------------------------------
    //
    // Sort and page size are GET forms, so every combination stays a shareable URL. Asking
    // the user to press "Apply" after choosing an order is a step with no decision in it.
    for (const select of document.querySelectorAll('select[data-role="autosubmit"]')) {
        select.addEventListener("change", function () {
            if (select.form) select.form.submit();
        });
    }

    // --- confirm the irreversible-ish ---------------------------------------------
    //
    // "or all N" tags every asset matching the search — thousands of rows from one click,
    // with no undo. §9.1's rule is that the human selects; a mis-click is not a selection.
    for (const button of document.querySelectorAll("[data-confirm]")) {
        button.addEventListener("click", function (event) {
            if (!window.confirm(button.dataset.confirm)) event.preventDefault();
        });
    }

    if (!grid) return;

    // --- hover plays the animation ------------------------------------------------
    //
    // §6 asked for this and the markup has carried `data-anim` since M2 with nothing
    // reading it: the template's comment claimed CSS handled it, and no rule ever did. The
    // still frame is remembered so leaving restores it rather than reloading the thumbnail.
    grid.addEventListener(
        "mouseenter",
        function (event) {
            const img = event.target;
            if (!(img instanceof HTMLImageElement) || !img.dataset.anim) return;
            if (!img.dataset.still) img.dataset.still = img.src;
            // A preview that will not load must not take the still frame down with it.
            // The server decides which assets get a data-anim and only offers files it
            // expects to exist, but "expects" is not "guarantees" — a confirmed frame
            // grid can outlive the sheet.gif built from the guess it replaced. Without
            // this the tile goes blank on hover and stays blank (M17).
            img.addEventListener(
                "error",
                function () {
                    if (img.dataset.still) img.src = img.dataset.still;
                    delete img.dataset.anim;
                },
                { once: true },
            );
            img.src = img.dataset.anim;
        },
        true,
    );
    grid.addEventListener(
        "mouseleave",
        function (event) {
            const img = event.target;
            if (!(img instanceof HTMLImageElement) || !img.dataset.still) return;
            img.src = img.dataset.still;
        },
        true,
    );

    // --- copy the local path (M16) ------------------------------------------------
    //
    // navigator.clipboard needs a secure context, and this runs on a plain-HTTP LAN, so the
    // hidden-textarea fallback is the path that actually gets used here — the same compromise
    // §8 forced on the palette's copy button.
    grid.addEventListener("click", async function (event) {
        const button = event.target instanceof Element
            ? event.target.closest("[data-copy-path]")
            : null;
        if (!button) return;
        event.preventDefault();

        const text = button.dataset.copyPath;
        let ok = false;
        if (navigator.clipboard && window.isSecureContext) {
            try {
                await navigator.clipboard.writeText(text);
                ok = true;
            } catch (e) {
                ok = false;
            }
        }
        if (!ok) {
            const scratch = document.createElement("textarea");
            scratch.value = text;
            scratch.setAttribute("readonly", "");
            scratch.className = "offscreen-copy";
            document.body.appendChild(scratch);
            scratch.select();
            try {
                ok = document.execCommand("copy");
            } catch (e) {
                ok = false;
            }
            document.body.removeChild(scratch);
        }
        if (ok) {
            button.classList.add("copied");
            window.setTimeout(() => button.classList.remove("copied"), 700);
        }
    });

    // --- selection ---------------------------------------------------------------
    //
    // Checkboxes only carry the page you are looking at, so ticking twelve sprites and then
    // turning the page used to lose them silently — the bulk tag would apply to whatever was
    // on screen at submit time. The ids are kept in sessionStorage against the current
    // search, and re-injected as hidden inputs on submit, so a selection spans pages and is
    // still gone when you start a new search or a new session.
    const boxes = () => Array.from(grid.querySelectorAll('input[type="checkbox"][name="id"]'));
    const storeKey = "ambar.selection:" + (window.location.search || "?");

    function loadSelection() {
        try {
            const raw = window.sessionStorage.getItem(storeKey);
            return new Set(raw ? JSON.parse(raw) : []);
        } catch (e) {
            return new Set();
        }
    }

    function saveSelection(set) {
        try {
            window.sessionStorage.setItem(storeKey, JSON.stringify(Array.from(set)));
        } catch (e) {
            // Private mode: selection then lasts one page, which is where it started.
        }
    }

    const selected = loadSelection();
    const counter = document.querySelector('[data-role="selected-count"]');
    const pageToggle = document.querySelector('[data-role="select-page"]');

    function paint() {
        for (const box of boxes()) {
            const on = selected.has(box.value);
            box.checked = on;
            const tile = box.closest(".tile");
            if (tile) tile.classList.toggle("tile-selected", on);
        }
        if (counter) counter.textContent = String(selected.size);
        if (pageToggle) {
            const all = boxes();
            pageToggle.checked = all.length > 0 && all.every((b) => selected.has(b.value));
        }
    }

    grid.addEventListener("change", function (event) {
        const box = event.target;
        if (!(box instanceof HTMLInputElement) || box.name !== "id") return;
        if (box.checked) selected.add(box.value);
        else selected.delete(box.value);
        saveSelection(selected);
        paint();
    });

    if (pageToggle) {
        pageToggle.addEventListener("change", function () {
            for (const box of boxes()) {
                if (pageToggle.checked) selected.add(box.value);
                else selected.delete(box.value);
            }
            saveSelection(selected);
            paint();
        });
    }

    // On submit, add the ids that are selected but not on this page. The checkboxes cover
    // the rest, and a duplicate id would be harmless anyway — the server works on a set.
    if (form) {
        form.addEventListener("submit", function (event) {
            const scope = event.submitter && event.submitter.value;
            if (scope === "all") return; // the server resolves that from the query

            const onPage = new Set(boxes().map((b) => b.value));
            for (const id of selected) {
                if (onPage.has(id)) continue;
                const hidden = document.createElement("input");
                hidden.type = "hidden";
                hidden.name = "id";
                hidden.value = id;
                form.appendChild(hidden);
            }
            // The work is about to be applied, so the selection has served its purpose.
            try {
                window.sessionStorage.removeItem(storeKey);
            } catch (e) {
                // Nothing to clear.
            }
        });
    }

    paint();

    // --- keyboard ----------------------------------------------------------------
    //
    // §8 asked for keyboard navigation in the grid and there was none. Arrow keys move a
    // focus ring tile to tile, Enter opens, Space selects — and none of it fires while focus
    // is in an input, because the search box and the tag field own the keyboard when they
    // have it.
    const tiles = () => Array.from(grid.querySelectorAll(".tile"));
    let cursor = -1;

    function focusTile(index) {
        const all = tiles();
        if (all.length === 0) return;
        cursor = Math.max(0, Math.min(all.length - 1, index));
        for (const [i, tile] of all.entries()) tile.classList.toggle("tile-cursor", i === cursor);
        const link = all[cursor].querySelector(".tilelink");
        if (link) link.focus({ preventScroll: false });
    }

    // How many tiles fit across, so up/down move a row rather than an item. Measured from
    // the laid-out grid instead of from --tile, which would not survive the size slider.
    function columns() {
        const all = tiles();
        if (all.length < 2) return 1;
        const top = all[0].getBoundingClientRect().top;
        let n = 0;
        for (const tile of all) {
            if (Math.abs(tile.getBoundingClientRect().top - top) > 2) break;
            n++;
        }
        return Math.max(1, n);
    }

    function isTyping(el) {
        if (!el) return false;
        if (el.isContentEditable) return true;
        const tag = el.tagName;
        return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
    }

    document.addEventListener("keydown", function (event) {
        if (event.ctrlKey || event.metaKey || event.altKey) return;
        if (isTyping(event.target)) return;

        const all = tiles();
        if (all.length === 0) return;

        switch (event.key) {
            case "ArrowRight":
                focusTile(cursor + 1);
                break;
            case "ArrowLeft":
                focusTile(cursor <= 0 ? 0 : cursor - 1);
                break;
            case "ArrowDown":
                focusTile(cursor < 0 ? 0 : cursor + columns());
                break;
            case "ArrowUp":
                focusTile(cursor < 0 ? 0 : cursor - columns());
                break;
            case "Home":
                focusTile(0);
                break;
            case "End":
                focusTile(all.length - 1);
                break;
            case " ": {
                if (cursor < 0) return;
                const box = all[cursor].querySelector('input[type="checkbox"][name="id"]');
                if (!box) return;
                box.checked = !box.checked;
                box.dispatchEvent(new Event("change", { bubbles: true }));
                break;
            }
            case "n":
            case "p": {
                // The pager, from the keyboard: the same links the footer renders.
                const link = document.querySelector(
                    '[data-role="' + (event.key === "n" ? "next-page" : "prev-page") + '"]',
                );
                if (!link) return;
                window.location.href = link.href;
                break;
            }
            default:
                return;
        }
        event.preventDefault();
    });
})();
