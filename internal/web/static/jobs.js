// Live background-work status (M16).
//
// §12 asked for pollable status; what existed was a page you reloaded by hand, and two other
// pages that told you to ("watch background work, then reload"). An application that instructs
// the user to press F5 has moved its own job onto them.
//
// One poller, two consumers: the sidebar's scan line on every workspace page, and the jobs
// page's own table. It polls only while something is running or queued, and stops when the
// queue goes idle — a NAS should not answer a request every two seconds to say "nothing is
// happening".
(function () {
    "use strict";

    const ACTIVE_MS = 2000;
    // A slow heartbeat while idle, so work started somewhere else — the inbox poller, another
    // browser, the nightly scan — still shows up without a reload.
    const IDLE_MS = 30000;

    const line = document.querySelector('[data-role="scan-line"]');
    const table = document.querySelector('[data-role="jobs-table"]');
    const scanForm = document.querySelector('[data-role="scan-form"]');
    const workersBody = document.querySelector('[data-role="workers-body"]');
    if (!line && !table && !scanForm && !workersBody) return;

    let timer = 0;
    let stopped = false;

    function labelFor(job) {
        // Job types are dotted identifiers; the second half is the useful word.
        const kind = String(job.type || "").split(".").pop();
        const parts = [kind];
        if (job.note) parts.push(job.note);
        if (job.total > 0) parts.push(job.done + " / " + job.total);
        else if (job.done > 0) parts.push(String(job.done));
        return parts.join(" · ");
    }

    // How long ago an ISO timestamp was, in the jobs table's own style.
    function ago(iso) {
        const seconds = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
        if (seconds < 60) return Math.floor(seconds) + "s ago";
        if (seconds < 3600) return Math.floor(seconds / 60) + "m ago";
        if (seconds < 86400) return Math.floor(seconds / 3600) + "h ago";
        return Math.floor(seconds / 86400) + "d ago";
    }

    function el(tag, className, text) {
        const node = document.createElement(tag);
        if (className) node.className = className;
        if (text !== undefined) node.textContent = text;
        return node;
    }

    // Remote derive workers (internal/remote): one row per worker, rebuilt from the poll.
    // Built with textContent throughout — a file name is library content, not markup.
    function paintWorkers(workers) {
        if (!workersBody) return;
        workersBody.replaceChildren();
        if (!workers || workers.length === 0) {
            const row = el("tr");
            const cell = el("td", "hint", "No worker has connected. Run ambar worker on a machine with the library mounted.");
            cell.colSpan = 5;
            row.append(cell);
            workersBody.append(row);
            return;
        }
        for (const w of workers) {
            const row = el("tr");

            const name = el("td");
            name.append(el("strong", "", w.name));
            const traits = [];
            if (w.blender) traits.push("Blender");
            if (w.concurrency) traits.push(w.concurrency + " slots");
            if (traits.length) name.append(" ", el("span", "hint", traits.join(" · ")));

            const status = el("td");
            if (w.connected) {
                status.append(el("span", "state state-running", "connected"), " ",
                    el("span", "hint", "since " + ago(w.since)));
            } else {
                status.append(el("span", "state", "gone"), " ",
                    el("span", "hint", "last seen " + ago(w.last_seen)));
            }

            const rate = el("td", "num", String(w.per_minute || 0));

            const done = el("td", "num", String(w.done || 0));
            done.title = "ok " + (w.ok || 0) + " · no decoder " + (w.unsupported || 0) +
                " · needs Blender " + (w.needs_blender || 0) + " · failed " + (w.failed || 0);
            if (w.failed) done.append(" ", el("span", "state state-failed", w.failed + " failed"));

            const current = el("td", "errorcell");
            if (w.current && w.current.length) {
                for (const job of w.current) {
                    const line = el("div");
                    line.append(el("code", "", job.file));
                    current.append(line);
                }
            } else {
                current.textContent = "—";
            }

            row.append(name, status, rate, done, current);
            workersBody.append(row);
        }
    }

    function paint(status) {
        paintWorkers(status.workers);
        if (line) {
            if (status.running > 0 || status.queued > 0) {
                const bits = [];
                // A worker running eight derives at once would otherwise be eight copies of
                // the same word; identical labels collapse into one with a count.
                const counts = new Map();
                for (const job of status.active || []) {
                    const label = labelFor(job) + (job.worker ? " on " + job.worker : "");
                    counts.set(label, (counts.get(label) || 0) + 1);
                }
                for (const [label, n] of counts) bits.push(n > 1 ? label + " ×" + n : label);
                if (status.local_paused) bits.push("this server paused");
                if (status.queued > 0) bits.push(status.queued + " queued");
                line.textContent = bits.join(" · ") || "working…";
                line.classList.add("is-working");
            } else {
                // Back to the resting state: when the library was last scanned.
                const parts = [];
                if (status.last_scan_ago) parts.push("Scanned " + status.last_scan_ago);
                if (status.last_scan_assets) parts.push(status.last_scan_assets + " assets");
                line.textContent = parts.join(" · ") || "Not scanned yet";
                line.classList.remove("is-working");
            }
        }

        // The jobs page reloads its own table when work finishes, because the interesting
        // change there is the *result* — what failed, what it said — and that is a full row.
        if (table && table.dataset.working === "true" && status.idle) {
            window.location.reload();
        }
        if (table) table.dataset.working = status.idle ? "false" : "true";
    }

    async function poll() {
        if (stopped) return;
        try {
            const response = await fetch("/api/v1/jobs/status", {
                headers: { "X-Requested-With": "fetch" },
            });
            if (response.ok) {
                const status = await response.json();
                paint(status);
                schedule(status.idle ? IDLE_MS : ACTIVE_MS);
                return;
            }
        } catch (e) {
            // Offline, or the server restarted mid-poll. Try again on the slow beat rather
            // than hammering it.
        }
        schedule(IDLE_MS);
    }

    function schedule(ms) {
        window.clearTimeout(timer);
        timer = window.setTimeout(poll, ms);
    }

    // The scan button (M16): it used to redirect to /jobs, which threw away the grid to show a
    // table that did not refresh either. Now it posts in place and the line starts moving.
    if (scanForm) {
        scanForm.addEventListener("submit", function (event) {
            event.preventDefault();
            const button = scanForm.querySelector("button");
            if (button) button.disabled = true;
            if (line) {
                line.textContent = "starting…";
                line.classList.add("is-working");
            }

            fetch(scanForm.action, {
                method: "POST",
                headers: {
                    "Content-Type": "application/x-www-form-urlencoded",
                    "X-Requested-With": "fetch",
                },
                body: new URLSearchParams(new FormData(scanForm)).toString(),
            })
                .catch(() => {
                    if (line) line.textContent = "could not start the scan";
                })
                .finally(() => {
                    if (button) button.disabled = false;
                    schedule(300);
                });
        });
    }

    // Stop when the tab is hidden: a background tab polling a NAS every two seconds is pure
    // cost, and the first thing that happens on return is a poll.
    document.addEventListener("visibilitychange", function () {
        if (document.hidden) {
            stopped = true;
            window.clearTimeout(timer);
        } else {
            stopped = false;
            schedule(0);
        }
    });

    // Paint the progress bars the server rendered. Their width is a CSS custom property set
    // here rather than an inline style attribute, because §11 keeps the CSP free of
    // 'unsafe-inline'.
    for (const bar of document.querySelectorAll(".jobprogress-bar[data-percent]")) {
        bar.style.setProperty("--pct", bar.dataset.percent + "%");
    }

    schedule(0);
})();
