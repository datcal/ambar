// The jobs page's "Model thumbnails" card: draw, in this tab, every model whose thumbnail
// is missing or was drawn by an older renderer (internal/server/rethumb.go).
//
// Model thumbnails come from a browser — the server has no 3D renderer — and until this
// card a browser only drew one when a tile scrolled into view, once. So the white FBX
// tiles drawn before textures were resolved by name stayed white. This walks the whole
// library a batch at a time with the grid's own renderer (modelthumb.js), on this
// computer's graphics card, and the server keeps only what is better than what it had.
(function () {
    "use strict";

    var card = document.querySelector('[data-role="rethumb"]');
    if (!card) return;
    var startButton = card.querySelector('[data-role="rethumb-start"]');
    var stopButton = card.querySelector('[data-role="rethumb-stop"]');
    var status = card.querySelector('[data-role="rethumb-status"]');
    var version = card.dataset.version || "";

    // The renderer and its loaders, fetched only when asked: the jobs page should not
    // download a megabyte of three.js to show a table.
    var SCRIPTS = [
        "vendor/three/three.min.js", "vendor/three/GLTFLoader.js", "vendor/three/OBJLoader.js",
        "vendor/three/MTLLoader.js", "vendor/three/fflate.min.js", "vendor/three/FBXLoader.js",
        "modeltextures.js", "modelthumb.js",
    ];
    // Where the walk got to, so stopping and coming back continues rather than restarts.
    // A per-browser convenience; the server needs none of it.
    var KEY = "ambar.rethumb.after";

    var running = false;
    var after = 0;
    var maxID = 0;
    var drawn = 0;
    var untextured = 0;
    var failed = 0;

    try { after = parseInt(window.localStorage.getItem(KEY) || "0", 10) || 0; } catch (e) { after = 0; }
    if (after > 0) status.textContent = "Stopped part-way; starting again continues from there.";

    function remember() {
        try { window.localStorage.setItem(KEY, String(after)); } catch (e) { /* private window */ }
    }

    function say(text) { status.textContent = text; }

    function buttons() {
        startButton.hidden = running;
        stopButton.hidden = !running;
    }

    function loadScripts(i, done) {
        if (i >= SCRIPTS.length) return done();
        var tag = document.createElement("script");
        tag.src = "/static/" + SCRIPTS[i] + "?v=" + encodeURIComponent(version);
        tag.onload = function () { loadScripts(i + 1, done); };
        tag.onerror = function () {
            say("Could not load the 3D renderer.");
            running = false;
            buttons();
        };
        document.head.appendChild(tag);
    }

    function progress(file) {
        var pct = maxID ? Math.min(99, Math.floor((after * 100) / maxID)) : 0;
        var bits = [drawn + " drawn"];
        if (untextured) bits.push(untextured + " still missing some textures");
        if (failed) bits.push(failed + " could not be drawn");
        say((file ? "Drawing " + file + " — " : "") + bits.join(", ") + " · " + pct + "% of the library checked");
    }

    function finished() {
        running = false;
        after = 0;
        remember();
        buttons();
        var bits = [drawn + " drawn"];
        if (untextured) bits.push(untextured + " still missing some textures");
        if (failed) bits.push(failed + " could not be drawn");
        say("Done: " + bits.join(", ") + ".");
    }

    function batch() {
        if (!running) return;
        fetch("/models/render-queue?limit=50&after=" + after, {
            headers: { "X-Requested-With": "fetch" },
            credentials: "same-origin",
        })
            .then(function (response) {
                if (!response.ok) throw new Error(String(response.status));
                return response.json();
            })
            .then(function (page) {
                maxID = page.max_id || maxID;
                var items = page.items || [];
                var i = 0;
                (function one() {
                    if (!running) return;
                    if (i >= items.length) {
                        after = page.after;
                        remember();
                        if (page.done) return finished();
                        progress("");
                        return batch();
                    }
                    var item = items[i++];
                    progress(item.src.split("/").pop());
                    window.AmbarModelThumb.render(item, function (stored, dropped) {
                        if (stored) {
                            drawn += 1;
                            if (dropped) untextured += 1;
                        } else {
                            failed += 1;
                        }
                        after = item.asset;
                        remember();
                        window.setTimeout(one, 20);
                    });
                })();
            })
            .catch(function () {
                running = false;
                buttons();
                say("The server did not answer, so drawing stopped. Starting again continues from here.");
            });
    }

    startButton.addEventListener("click", function () {
        if (running) return;
        running = true;
        drawn = untextured = failed = 0;
        buttons();
        say("Loading the 3D renderer…");
        var begin = function () {
            if (!window.AmbarModelThumb) {
                running = false;
                buttons();
                say("The 3D renderer did not start (WebGL may be unavailable in this browser).");
                return;
            }
            progress("");
            batch();
        };
        if (window.AmbarModelThumb) begin();
        else loadScripts(0, begin);
    });

    stopButton.addEventListener("click", function () {
        running = false;
        buttons();
        say("Stopped after " + drawn + " drawn. Starting again continues from here.");
    });
})();
