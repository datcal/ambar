// The folder view's interactions: copying a path, keeping the open folder in sight, the
// jump-to-folder box, and the asset panel.
//
// An island like the rest: no framework, everything from data- attributes, nothing inline,
// so the CSP stays `default-src 'self'`. Each part degrades on its own. Without JavaScript the
// jump box is a GET to /go (which takes a folder's name as well as a path), a tile is a link
// to the asset's page in a new tab, and the copy buttons are the only thing that goes inert.
(function () {
    "use strict";

    // --- copy a path -------------------------------------------------------------
    //
    // One handler for every [data-copy-path] on the page: the tiles, the folder header and
    // the panel. navigator.clipboard needs a secure context and this runs on a plain-HTTP
    // LAN, so the hidden-textarea fallback is the path that actually gets used.
    async function copyText(text) {
        if (navigator.clipboard && window.isSecureContext) {
            try {
                await navigator.clipboard.writeText(text);
                return true;
            } catch (e) {
                // Fall through to the textarea.
            }
        }
        const scratch = document.createElement("textarea");
        scratch.value = text;
        scratch.setAttribute("readonly", "");
        scratch.className = "offscreen-copy";
        document.body.appendChild(scratch);
        scratch.select();
        let ok = false;
        try {
            ok = document.execCommand("copy");
        } catch (e) {
            ok = false;
        }
        document.body.removeChild(scratch);
        return ok;
    }

    document.addEventListener("click", async function (event) {
        const button = event.target instanceof Element ? event.target.closest("[data-copy-path]") : null;
        if (!button) return;
        event.preventDefault();
        const ok = await copyText(button.dataset.copyPath);
        if (!ok) return;
        button.classList.add("copied");
        // A worded button says so; the tile's glyph button only flashes.
        if (!button.classList.contains("tileact")) {
            const before = button.textContent;
            button.textContent = "Copied";
            window.setTimeout(function () {
                button.textContent = before;
                button.classList.remove("copied");
            }, 1200);
        } else {
            window.setTimeout(() => button.classList.remove("copied"), 700);
        }
    });

    // --- the open folder, in sight --------------------------------------------------
    //
    // A vendor folder has a couple of hundred packs, so the one you are in is usually far
    // below the fold. Scroll the sidebar — only the sidebar — to it.
    const current = document.querySelector(".tree .tree-row.on");
    if (current) {
        const panel = current.closest(".panel");
        if (panel) {
            const top = current.getBoundingClientRect().top - panel.getBoundingClientRect().top;
            if (top < 0 || top > panel.clientHeight - 40) {
                panel.scrollTop += top - panel.clientHeight / 3;
            }
        }
    }

    // --- folds that load when opened ------------------------------------------------
    //
    // The sidebar's Tags section counts the tags of the current result set, which is a
    // second's work on a large folder; it is done when somebody opens the fold, once.
    for (const fold of document.querySelectorAll('details[data-role="lazy-fold"]')) {
        fold.addEventListener("toggle", async function () {
            if (!fold.open || fold.dataset.loaded) return;
            fold.dataset.loaded = "1";
            const body = fold.querySelector('[data-role="lazy-body"]');
            try {
                const response = await fetch(fold.dataset.src, { headers: { "X-Requested-With": "fetch" } });
                if (!response.ok) throw new Error(String(response.status));
                body.innerHTML = await response.text();
            } catch (e) {
                body.innerHTML = '<p class="panel-note">The tags could not be loaded.</p>';
                delete fold.dataset.loaded;
            }
        });
    }

    // --- go to a path ----------------------------------------------------------------
    //
    // Opening "Go to path…" selects the current path, so a paste replaces it and the arrow
    // keys edit it.
    const gopath = document.querySelector("details.gopath");
    if (gopath) {
        gopath.addEventListener("toggle", function () {
            if (!gopath.open) return;
            const input = gopath.querySelector('[data-role="gopath-input"]');
            if (input) {
                input.focus();
                input.select();
            }
        });
    }

    // --- jump to a folder -------------------------------------------------------------
    const jumpForm = document.querySelector('[data-role="jump-form"]');
    if (jumpForm) {
        const input = jumpForm.querySelector('[data-role="jump-input"]');
        const hits = jumpForm.querySelector('[data-role="jump-hits"]');
        let timer = 0;
        let inFlight = null;
        let active = -1;

        const items = () => Array.from(hits.querySelectorAll(".jump-item"));
        const show = () => {
            hits.hidden = false;
            input.setAttribute("aria-expanded", "true");
        };
        const hide = () => {
            hits.hidden = true;
            input.setAttribute("aria-expanded", "false");
            active = -1;
        };
        const highlight = (index) => {
            const all = items();
            if (all.length === 0) return;
            active = (index + all.length) % all.length;
            all.forEach((el, i) => {
                el.classList.toggle("on", i === active);
                el.setAttribute("aria-selected", i === active ? "true" : "false");
                if (i === active) el.scrollIntoView({ block: "nearest" });
            });
        };

        async function find() {
            const query = input.value.trim();
            if (!query) {
                hide();
                return;
            }
            if (inFlight) inFlight.abort();
            const controller = new AbortController();
            inFlight = controller;
            const params = new URLSearchParams();
            params.set("q", query);
            const kind = jumpForm.querySelector('input[name="kind"]');
            if (kind) params.set("kind", kind.value);
            // The view settings travel too, so a jump keeps the list view and the sort.
            const here = new URLSearchParams(window.location.search);
            for (const key of ["view", "sort", "per"]) {
                if (here.get(key)) params.set(key, here.get(key));
            }
            try {
                const response = await fetch("/folders/find?" + params.toString(), {
                    signal: controller.signal,
                    headers: { "X-Requested-With": "fetch" },
                });
                if (!response.ok) return;
                hits.innerHTML = await response.text();
                active = -1;
                show();
                if (items().length > 0) highlight(0);
            } catch (e) {
                // Aborted by the next keystroke, or offline.
            }
        }

        input.addEventListener("input", function () {
            window.clearTimeout(timer);
            timer = window.setTimeout(find, 120);
        });
        input.addEventListener("keydown", function (event) {
            switch (event.key) {
                case "ArrowDown":
                    if (hits.hidden) return;
                    event.preventDefault();
                    highlight(active + 1);
                    break;
                case "ArrowUp":
                    if (hits.hidden) return;
                    event.preventDefault();
                    highlight(active - 1);
                    break;
                case "Enter": {
                    // A highlighted row goes straight there; otherwise the form submits to
                    // /go, which also understands a pasted path.
                    const el = items()[active];
                    if (!hits.hidden && el) {
                        event.preventDefault();
                        window.location.href = el.href;
                    }
                    break;
                }
                case "Escape":
                    hide();
                    break;
                default:
                    break;
            }
        });
        input.addEventListener("blur", function () {
            // A moment, so a click on a row still lands.
            window.setTimeout(hide, 150);
        });
    }

    // --- the asset panel ------------------------------------------------------------
    //
    // A click on a tile shows the asset beside the grid instead of opening a tab: what it
    // is, its formats, which file Godot and which Unreal should take, its path, where it is
    // used. A double click, a middle click or Ctrl/Cmd+click still opens the full page in a
    // new tab, because the link underneath is unchanged. With the panel open, moving through
    // the grid with the arrow keys moves the panel along.
    const rail = document.querySelector('[data-role="peek"]');
    const grid = document.getElementById("thumbgrid");
    if (!rail || !grid) return;

    let shown = null;
    let loading = null;
    let focusTimer = 0;

    function markTile(tile) {
        for (const el of grid.querySelectorAll(".tile-peeked")) el.classList.remove("tile-peeked");
        if (tile) tile.classList.add("tile-peeked");
    }

    async function openPeek(link) {
        const url = link.dataset.peek;
        if (!url || url === shown) return;
        shown = url;
        markTile(link.closest(".tile"));
        rail.hidden = false;
        document.body.classList.add("has-peek");
        rail.setAttribute("aria-busy", "true");

        if (loading) loading.abort();
        const controller = new AbortController();
        loading = controller;
        try {
            const response = await fetch(url, {
                signal: controller.signal,
                headers: { "X-Requested-With": "fetch" },
            });
            if (!response.ok) {
                rail.innerHTML = '<p class="hint">This asset could not be loaded.</p>';
                return;
            }
            rail.innerHTML = await response.text();
            rail.scrollTop = 0;
            borrowTilePicture(link);
        } catch (e) {
            // Superseded by another tile.
        } finally {
            if (loading === controller) {
                loading = null;
                rail.removeAttribute("aria-busy");
            }
        }
    }

    // A model whose thumbnail the browser is still being asked to draw (M15) has no
    // picture on the server yet, but the tile may already show the one this browser drew.
    // Borrow it rather than putting "FBX" in a large grey square.
    function borrowTilePicture(link) {
        const placeholder = rail.querySelector(".peek-media .thumb-none");
        if (!placeholder) return;
        const drawn = link.querySelector(".thumb img, .thumb canvas");
        if (!drawn) return;
        const img = document.createElement("img");
        img.alt = "";
        img.src = drawn instanceof HTMLCanvasElement ? drawn.toDataURL() : drawn.src;
        placeholder.replaceWith(img);
    }

    function closePeek() {
        shown = null;
        rail.hidden = true;
        rail.innerHTML = "";
        document.body.classList.remove("has-peek");
        markTile(null);
    }

    const plainClick = (event) =>
        event.button === 0 && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey;

    grid.addEventListener("click", function (event) {
        const link = event.target instanceof Element ? event.target.closest("a.tilelink") : null;
        if (!link || !plainClick(event)) return;
        event.preventDefault();
        openPeek(link);
    });

    grid.addEventListener("dblclick", function (event) {
        const link = event.target instanceof Element ? event.target.closest("a.tilelink") : null;
        if (!link) return;
        event.preventDefault();
        window.open(link.href, "_blank", "noopener");
    });

    grid.addEventListener("focusin", function (event) {
        if (rail.hidden) return;
        const link = event.target instanceof Element ? event.target.closest("a.tilelink") : null;
        if (!link) return;
        window.clearTimeout(focusTimer);
        focusTimer = window.setTimeout(() => openPeek(link), 90);
    });

    rail.addEventListener("click", function (event) {
        const close = event.target instanceof Element ? event.target.closest('[data-role="peek-close"]') : null;
        if (close) closePeek();
    });

    document.addEventListener("keydown", function (event) {
        if (event.key !== "Escape" || rail.hidden) return;
        const el = document.activeElement;
        if (el && ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName)) return;
        closePeek();
    });
})();
