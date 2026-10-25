// The search box's completion (M16).
//
// There was none: the toolbar's box had no suggestions at all, so searching a 6,000-asset
// library meant remembering both its vocabulary and the query syntax. The only completion in
// the application was a `<datalist>` on the tag inputs, and a datalist cannot be styled,
// grouped, annotated with counts, or navigated predictably — which is why this is a real
// listbox instead.
//
// Four behaviours, in the order they matter:
//
//   - A scope. The folder and kind being browsed sit in the box as chips (base.html), and
//     the suggestions can move them: a folder row makes that folder the scope — type "spy",
//     Enter, and you are searching inside POLYGON - Spy Kit — and a kind row narrows to it,
//     counted where you are. Backspace in an empty box drops the last chip.
//   - Completion of the token being typed. The server returns a grouped fragment; choosing a
//     row replaces only that token, so `type:model tur` → `type:model turret.glb` rather
//     than losing what you had already narrowed to.
//   - Recent searches when the box is empty. Held here rather than server-side: what one
//     browser typed is not library state, and §11 keeps the server out of it.
//   - `/` to focus, because the search is how the library is used.
//
// Without JavaScript the box is still a plain GET form that searches.
(function () {
    "use strict";

    const form = document.getElementById("search-form");
    if (!form) return;

    const input = form.querySelector('[data-role="search-input"]');
    const panel = form.querySelector('[data-role="suggest"]');
    if (!input || !panel) return;

    const RECENT_KEY = "ambar.recentSearches";
    const RECENT_MAX = 8;
    const DEBOUNCE_MS = 140;

    // --- recent searches ----------------------------------------------------------

    function readRecent() {
        try {
            const raw = window.localStorage.getItem(RECENT_KEY);
            const list = raw ? JSON.parse(raw) : [];
            return Array.isArray(list) ? list.filter((s) => typeof s === "string") : [];
        } catch (e) {
            return [];
        }
    }

    function rememberSearch(query) {
        query = query.trim();
        if (!query) return;
        const list = readRecent().filter((s) => s !== query);
        list.unshift(query);
        try {
            window.localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, RECENT_MAX)));
        } catch (e) {
            // Private mode. Completion still works; only the history is lost.
        }
    }

    form.addEventListener("submit", function () {
        rememberSearch(input.value);
    });

    function renderRecent() {
        const list = readRecent();
        if (list.length === 0) {
            hide();
            return;
        }
        const ul = document.createElement("ul");
        ul.className = "suggest-list";
        ul.setAttribute("role", "listbox");

        const heading = document.createElement("li");
        heading.className = "suggest-group";
        heading.setAttribute("role", "presentation");
        heading.textContent = "Recent";
        ul.appendChild(heading);

        for (const query of list) {
            const li = document.createElement("li");
            li.setAttribute("role", "presentation");
            const button = document.createElement("button");
            button.type = "button";
            button.className = "suggest-item";
            button.setAttribute("role", "option");
            button.tabIndex = -1;
            // A whole past query replaces the box, not just the last token.
            button.dataset.replace = query;
            const text = document.createElement("span");
            text.className = "suggest-text";
            text.textContent = query;
            button.appendChild(text);
            li.appendChild(button);
            ul.appendChild(li);
        }

        panel.replaceChildren(ul);
        show();
    }

    // --- the panel ----------------------------------------------------------------

    let active = -1;

    function items() {
        return Array.from(panel.querySelectorAll(".suggest-item"));
    }

    function show() {
        panel.hidden = false;
        input.setAttribute("aria-expanded", "true");
    }

    function hide() {
        panel.hidden = true;
        input.setAttribute("aria-expanded", "false");
        active = -1;
    }

    function highlight(index) {
        const all = items();
        if (all.length === 0) return;
        active = (index + all.length) % all.length;
        for (const [i, el] of all.entries()) {
            const on = i === active;
            el.classList.toggle("on", on);
            el.setAttribute("aria-selected", on ? "true" : "false");
            if (on) el.scrollIntoView({ block: "nearest" });
        }
    }

    // choose applies a row. `data-href` goes somewhere (a folder or a kind: the scope
    // changes, not the text); `data-replace` swaps the whole query (a recent search);
    // `data-insert` replaces only the token being typed.
    function choose(el) {
        if (el.dataset.href !== undefined) {
            hide();
            window.location.href = el.dataset.href;
            return;
        }
        if (el.dataset.replace !== undefined) {
            input.value = el.dataset.replace;
        } else {
            const value = input.value;
            const cut = value.lastIndexOf(" ");
            const prefix = cut < 0 ? "" : value.slice(0, cut + 1);
            input.value = prefix + el.dataset.insert;
            // A field keyword ends in ":" and needs a value, so stay in the box; anything
            // else is a complete term and the user meant to search for it.
            if (el.dataset.insert.endsWith(":")) {
                hide();
                input.focus();
                return;
            }
        }
        hide();
        form.submit();
    }

    panel.addEventListener("mousedown", function (event) {
        // mousedown rather than click: the input's blur would hide the panel first.
        const el = event.target instanceof Element ? event.target.closest(".suggest-item") : null;
        if (!el) return;
        event.preventDefault();
        choose(el);
    });

    // --- fetching -----------------------------------------------------------------

    let timer = 0;
    let inFlight = null;

    // The scope the box is searching in, from its own hidden fields: what the server needs
    // to count kinds in the right folder and to build links that keep the view.
    function scopeParams() {
        const params = new URLSearchParams();
        for (const name of ["dir", "kind", "sub", "view"]) {
            const field = form.querySelector('input[type="hidden"][name="' + name + '"]');
            if (field && field.value) params.set(name, field.value);
        }
        const here = new URLSearchParams(window.location.search);
        for (const name of ["sort", "per"]) {
            if (here.get(name)) params.set(name, here.get(name));
        }
        return params;
    }

    async function fetchSuggestions() {
        const query = input.value;
        const scope = scopeParams();
        // An empty box offers recent searches — unless it is scoped to a folder with no kind
        // yet, where the kinds in that folder are the more useful next step.
        if (query.trim() === "" && !(scope.has("dir") && !scope.has("kind"))) {
            renderRecent();
            return;
        }

        // Abort the previous request: on a fast typist the answers can arrive out of order,
        // and a stale list under a fresh prefix is worse than no list.
        if (inFlight) inFlight.abort();
        const controller = new AbortController();
        inFlight = controller;

        try {
            scope.set("q", query);
            const response = await fetch("/api/v1/suggest?" + scope.toString(), {
                signal: controller.signal,
                headers: { "X-Requested-With": "fetch" },
            });
            if (!response.ok) {
                hide();
                return;
            }
            const html = await response.text();
            panel.innerHTML = html;
            if (items().length === 0) {
                if (query.trim() === "") renderRecent();
                else hide();
                return;
            }
            active = -1;
            show();
            // A scope row first in the list is almost always what Enter should do: "spy" ⏎
            // means the folder, not a text search for the letters.
            const first = items()[0];
            if (first && first.dataset.href !== undefined) highlight(0);
        } catch (e) {
            // Aborted or offline: leave whatever is on screen alone.
        }
    }

    input.addEventListener("input", function () {
        window.clearTimeout(timer);
        timer = window.setTimeout(fetchSuggestions, DEBOUNCE_MS);
    });

    input.addEventListener("focus", function () {
        if (input.value.trim() === "") fetchSuggestions();
    });

    input.addEventListener("blur", function () {
        // A moment, so a mousedown on a row still lands.
        window.setTimeout(hide, 120);
    });

    input.addEventListener("keydown", function (event) {
        switch (event.key) {
            case "ArrowDown":
                if (panel.hidden) {
                    fetchSuggestions();
                    return;
                }
                event.preventDefault();
                highlight(active + 1);
                break;
            case "ArrowUp":
                if (panel.hidden) return;
                event.preventDefault();
                highlight(active - 1);
                break;
            case "Enter": {
                if (panel.hidden || active < 0) return; // submit the query as typed
                const el = items()[active];
                if (!el) return;
                event.preventDefault();
                choose(el);
                break;
            }
            case "Escape":
                if (panel.hidden) return;
                event.preventDefault();
                hide();
                break;
            case "Backspace": {
                // An empty box: widen the search by dropping the last chip, the way a
                // token field does. The chip's own × link is where that goes.
                if (input.value !== "") return;
                const chips = form.querySelectorAll('[data-role="chip-x"]');
                if (chips.length === 0) return;
                event.preventDefault();
                window.location.href = chips[chips.length - 1].href;
                break;
            }
            case "Tab":
                // Tab completes the highlighted row without searching, which is what a
                // shell does and what muscle memory expects.
                if (panel.hidden || active < 0) return;
                event.preventDefault();
                {
                    const el = items()[active];
                    if (el && el.dataset.insert !== undefined) {
                        const value = input.value;
                        const cut = value.lastIndexOf(" ");
                        const prefix = cut < 0 ? "" : value.slice(0, cut + 1);
                        input.value = prefix + el.dataset.insert;
                        hide();
                    }
                }
                break;
            default:
                break;
        }
    });

    // --- "/" focuses the search ----------------------------------------------------

    document.addEventListener("keydown", function (event) {
        if (event.key !== "/" || event.ctrlKey || event.metaKey || event.altKey) return;
        const el = document.activeElement;
        if (el && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName))) return;
        event.preventDefault();
        input.focus();
        input.select();
    });
})();
