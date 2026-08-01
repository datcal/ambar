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
    if (!line && !table && !scanForm) return;

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

    function paint(status) {
        if (line) {
            if (status.running > 0 || status.queued > 0) {
                const bits = [];
                for (const job of status.active || []) bits.push(labelFor(job));
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
