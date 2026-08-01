// Drag-and-drop upload with a real progress bar (M16).
//
// The old form posted through htmx, which cannot report upload progress — so a five-minute
// upload of an itch.io pack was a page that appeared to do nothing. This uses XMLHttpRequest
// for one reason: `xhr.upload.onprogress` is still the only way in a browser to know how much
// of a request body has gone out. `fetch` cannot tell you.
//
// The flow matches the questions:
//
//   1. Drop. Each file uploads on its own, in sequence — the NAS writes to one disk, and four
//      parallel streams make all four slower and the progress meaningless.
//   2. Watch. Percentage, rate and remaining time, from bytes actually acknowledged.
//   3. Where does it go, and where did it come from? The server has looked inside the archive
//      by then, so the destination arrives with a suggestion already chosen; the source link
//      sits beside it, plainly optional. Both in one step because the ingest job carries both
//      — asking for the link after starting the extraction would have nowhere to put it.
(function () {
    "use strict";

    const root = document.getElementById("uploader");
    if (!root) return;

    const input = root.querySelector('[data-role="file-input"]');
    const zone = root.querySelector('[data-role="dropzone"]');
    const list = root.querySelector('[data-role="uploads"]');
    const fallback = root.querySelector('[data-role="fallback-form"]');
    if (!input || !zone || !list) return;

    // JavaScript is here, so the plain form must not also submit on Enter.
    if (fallback) fallback.addEventListener("submit", (event) => event.preventDefault());

    const folders = (root.dataset.folders || "").split(",").filter(Boolean);
    const maxBytes = Number(root.dataset.max || 0);
    const csrf = document.querySelector('meta[name="csrf-token"]');
    const csrfToken = csrf ? csrf.content : "";

    const queue = [];
    let busy = false;

    // --- helpers -----------------------------------------------------------------

    function humanBytes(n) {
        const units = ["B", "KB", "MB", "GB", "TB"];
        let value = n;
        let i = 0;
        while (value >= 1024 && i < units.length - 1) {
            value /= 1024;
            i++;
        }
        return (value < 10 && i > 0 ? value.toFixed(1) : Math.round(value)) + " " + units[i];
    }

    function humanDuration(seconds) {
        if (!isFinite(seconds) || seconds < 0) return "";
        if (seconds < 60) return Math.max(1, Math.round(seconds)) + "s";
        const minutes = Math.floor(seconds / 60);
        return minutes + "m " + Math.round(seconds % 60) + "s";
    }

    function el(tag, className, text) {
        const node = document.createElement(tag);
        if (className) node.className = className;
        if (text !== undefined) node.textContent = text;
        return node;
    }

    // --- one row per file --------------------------------------------------------

    function addRow(file) {
        const row = el("li", "upload");

        const head = el("div", "upload-head");
        head.appendChild(el("span", "upload-name", file.name));
        const status = el("span", "upload-status", "waiting");
        head.appendChild(status);
        row.appendChild(head);

        const track = el("div", "upload-track");
        const bar = el("div", "upload-bar");
        track.appendChild(bar);
        row.appendChild(track);

        const body = el("div", "upload-body");
        row.appendChild(body);

        list.appendChild(row);
        return { row, bar, status, body };
    }

    // askDestination renders step 3: where does this pack go, and (optionally) where did it
    // come from. Both in one step on purpose — the first version asked for the link *after*
    // starting the extraction, by which point the ingest job was already queued with an empty
    // source and there was nowhere to put the answer.
    function askDestination(ui, answer) {
        ui.body.replaceChildren();

        const form = el("form", "upload-dest");
        form.addEventListener("submit", (event) => {
            event.preventDefault();
            start();
        });

        const label = el("span", "upload-dest-label", "Extract into");
        form.appendChild(label);

        const select = document.createElement("select");
        for (const folder of answer.folders || folders) {
            const option = document.createElement("option");
            option.value = folder;
            option.textContent = folder;
            if (folder === answer.suggested) option.selected = true;
            select.appendChild(option);
        }
        const newOption = document.createElement("option");
        newOption.value = "__new__";
        newOption.textContent = "new folder…";
        select.appendChild(newOption);
        form.appendChild(select);

        const newName = document.createElement("input");
        newName.type = "text";
        newName.placeholder = "folder name";
        newName.className = "upload-newfolder";
        newName.hidden = true;
        form.appendChild(newName);

        select.addEventListener("change", () => {
            newName.hidden = select.value !== "__new__";
            if (!newName.hidden) newName.focus();
        });

        const go = el("button", "primary", "Extract");
        go.type = "submit";
        form.appendChild(go);

        ui.body.appendChild(form);

        // The link, on its own line and clearly optional: it is provenance, not a gate
        // between you and a file that has already uploaded.
        const sourceRow = el("div", "upload-source");
        const source = document.createElement("input");
        source.type = "url";
        source.placeholder = "https://… where you got it (optional)";
        sourceRow.appendChild(source);
        ui.body.appendChild(sourceRow);

        // What the server found inside, so the suggestion is not a black box.
        const facts = [];
        if (answer.file_count) facts.push(answer.file_count + " files");
        if (answer.suggest_reason) facts.push(answer.suggest_reason);
        if (!answer.suggested) facts.push("no clear kind — pick one");
        if (facts.length) ui.body.appendChild(el("p", "hint", facts.join(" · ")));

        function start() {
            const dest = select.value === "__new__" ? newName.value.trim() : select.value;
            if (select.value === "__new__" && dest === "") {
                newName.focus();
                return;
            }
            go.disabled = true;

            const body = new URLSearchParams();
            body.set("archive_rel_path", answer.archive_rel_path);
            body.set("dest", dest);
            body.set("source", source.value.trim());

            fetch("/ingest/start", {
                method: "POST",
                headers: {
                    "Content-Type": "application/x-www-form-urlencoded",
                    "X-CSRF-Token": csrfToken,
                },
                body: body.toString(),
            })
                .then((response) => {
                    if (!response.ok) throw new Error("start failed");
                    return response.json();
                })
                .then(() => {
                    ui.status.textContent = "extracting";
                    showQueued(ui, dest);
                })
                .catch(() => {
                    go.disabled = false;
                    ui.status.textContent = "could not start";
                    ui.status.classList.add("is-error");
                });
        }
    }

    // showQueued is the last step: the archive is queued, and the only thing left to say is
    // where to watch it.
    function showQueued(ui, dest) {
        ui.body.replaceChildren();
        ui.bar.style.width = "100%";
        ui.row.classList.add("upload-done");

        ui.body.appendChild(
            el("p", "hint", "Queued for extraction into " + (dest || "the library root") + "."),
        );

        const jobs = el("p", "hint");
        const link = el("a", null, "Background work →");
        link.href = "/jobs";
        jobs.appendChild(link);
        ui.body.appendChild(jobs);
    }

    // --- the upload itself -------------------------------------------------------

    function upload(file) {
        const ui = addRow(file);

        if (maxBytes > 0 && file.size > maxBytes) {
            ui.status.textContent = "too large (" + humanBytes(file.size) + ")";
            ui.status.classList.add("is-error");
            ui.body.appendChild(
                el("p", "hint", "Over the configured limit. Copy it into _inbox/ instead."),
            );
            return Promise.resolve();
        }

        return new Promise((resolve) => {
            const form = new FormData();
            form.append("archive", file);

            const xhr = new XMLHttpRequest();
            xhr.open("POST", "/ingest/upload");
            xhr.setRequestHeader("X-CSRF-Token", csrfToken);

            const startedAt = Date.now();
            xhr.upload.addEventListener("progress", (event) => {
                if (!event.lengthComputable) {
                    ui.status.textContent = "uploading " + humanBytes(event.loaded);
                    return;
                }
                const fraction = event.loaded / event.total;
                ui.bar.style.width = (fraction * 100).toFixed(1) + "%";

                const elapsed = (Date.now() - startedAt) / 1000;
                const rate = elapsed > 0 ? event.loaded / elapsed : 0;
                const remaining = rate > 0 ? (event.total - event.loaded) / rate : Infinity;
                ui.status.textContent =
                    Math.round(fraction * 100) +
                    "% · " +
                    humanBytes(rate) +
                    "/s" +
                    (isFinite(remaining) ? " · " + humanDuration(remaining) + " left" : "");
            });

            xhr.addEventListener("load", () => {
                if (xhr.status !== 200) {
                    ui.status.textContent = "failed";
                    ui.status.classList.add("is-error");
                    ui.body.appendChild(el("p", "hint", xhr.responseText.trim() || "Upload failed."));
                    resolve();
                    return;
                }
                let answer;
                try {
                    answer = JSON.parse(xhr.responseText);
                } catch (e) {
                    ui.status.textContent = "unexpected reply";
                    ui.status.classList.add("is-error");
                    resolve();
                    return;
                }
                ui.bar.style.width = "100%";
                ui.status.textContent = "uploaded " + humanBytes(answer.bytes || file.size);
                askDestination(ui, answer);
                resolve();
            });

            xhr.addEventListener("error", () => {
                ui.status.textContent = "network error";
                ui.status.classList.add("is-error");
                resolve();
            });

            xhr.addEventListener("abort", () => resolve());

            ui.status.textContent = "0%";
            xhr.send(form);
        });
    }

    async function drain() {
        if (busy) return;
        busy = true;
        while (queue.length > 0) {
            await upload(queue.shift());
        }
        busy = false;
    }

    function accept(files) {
        for (const file of files) queue.push(file);
        drain();
    }

    // --- wiring ------------------------------------------------------------------

    input.addEventListener("change", () => {
        accept(Array.from(input.files || []));
        input.value = ""; // so dropping the same file twice still fires
    });

    // The whole page is a drop target, not just the box: aiming at a rectangle is work, and
    // the browser's default for a dropped file is to navigate away from the page entirely.
    for (const type of ["dragenter", "dragover"]) {
        document.addEventListener(type, (event) => {
            if (!event.dataTransfer || !Array.from(event.dataTransfer.types).includes("Files")) return;
            event.preventDefault();
            zone.classList.add("is-over");
        });
    }
    for (const type of ["dragleave", "dragend"]) {
        document.addEventListener(type, (event) => {
            if (event.relatedTarget) return;
            zone.classList.remove("is-over");
        });
    }
    document.addEventListener("drop", (event) => {
        if (!event.dataTransfer) return;
        event.preventDefault();
        zone.classList.remove("is-over");
        accept(Array.from(event.dataTransfer.files || []));
    });
})();
