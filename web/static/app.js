(function () {
    "use strict";

    const $ = (selector, root) => (root || document).querySelector(selector);
    const $$ = (selector, root) => Array.from((root || document).querySelectorAll(selector));
    const LOCAL_KEY = "interim.uploads.v1";
    const UPLOAD_HASH_KEY = "interim.uploadHash.v1";
    const MAX_LOCAL = 200;

    function toast(message) {
        const el = $("#toast");
        if (!el) return;
        el.textContent = message;
        el.hidden = false;
        window.clearTimeout(toast.timer);
        toast.timer = window.setTimeout(() => {
            el.hidden = true;
        }, 3200);
    }

    async function copyText(value) {
        try {
            await navigator.clipboard.writeText(value);
        } catch (err) {
            const area = document.createElement("textarea");
            area.value = value;
            area.setAttribute("readonly", "");
            area.style.position = "fixed";
            area.style.opacity = "0";
            document.body.appendChild(area);
            area.select();
            document.execCommand("copy");
            area.remove();
        }
        toast("Copied");
    }

    function formatBytes(bytes) {
        const value = Number(bytes) || 0;
        if (value < 1024) return value + " B";
        const units = ["KB", "MB", "GB", "TB", "PB"];
        let number = value;
        let unit = "B";
        for (const next of units) {
            number /= 1024;
            unit = next;
            if (number < 1024) break;
        }
        if (number >= 100) return number.toFixed(0) + " " + unit;
        if (number >= 10) return number.toFixed(1) + " " + unit;
        return number.toFixed(2) + " " + unit;
    }

    function formatDate(iso) {
        const date = new Date(iso);
        if (Number.isNaN(date.getTime())) return "unknown";
        return date.toLocaleString();
    }

    function addCopyButtons() {
        $$("pre").forEach((pre) => {
            if (pre.querySelector(".copy-code")) return;
            const code = pre.querySelector("code");
            if (!code) return;
            const button = document.createElement("button");
            button.type = "button";
            button.className = "link-button copy-code";
            button.textContent = "Copy";
            button.addEventListener("click", (event) => {
                event.preventDefault();
                copyText(code.textContent);
            });
            pre.appendChild(button);
        });
    }

    document.addEventListener("click", (event) => {
        const target = event.target.closest("[data-copy]");
        if (!target) return;
        event.preventDefault();
        copyText(target.getAttribute("data-copy") || "");
    });

    function initConfirmForms() {
        $$("form[data-confirm]").forEach((form) => {
            form.addEventListener("submit", (event) => {
                if (!window.confirm(form.getAttribute("data-confirm") || "Are you sure?")) {
                    event.preventDefault();
                }
            });
        });
    }

    function initDownloadPage() {
        const button = $("#download-button");
        if (button) {
            button.addEventListener("click", () => {
                button.textContent = "Creating signed link…";
                button.classList.add("is-loading");
            });
        }

        const countdown = $("[data-countdown]");
        if (countdown) {
            const value = $("#countdown-value");
            const expires = Number(countdown.getAttribute("data-expires")) * 1000;
            let reloaded = false;
            const tick = () => {
                const remaining = expires - Date.now();
                if (remaining <= 0) {
                    if (value) value.textContent = "expired";
                    if (!reloaded) {
                        reloaded = true;
                        window.setTimeout(() => window.location.reload(), 1200);
                    }
                    return;
                }
                if (value) {
                    const totalMinutes = Math.floor(remaining / 60000);
                    const days = Math.floor(totalMinutes / 1440);
                    const hours = Math.floor((totalMinutes % 1440) / 60);
                    const minutes = totalMinutes % 60;
                    if (days > 0) value.textContent = days + "d " + hours + "h " + minutes + "m";
                    else if (hours > 0) value.textContent = hours + "h " + minutes + "m";
                    else value.textContent = minutes + "m";
                }
                window.setTimeout(tick, 30000);
            };
            tick();
        }
    }

    function initAdmin() {
        const selectAll = $("#select-all");
        const form = $("#bulk-form");
        if (!selectAll || !form) return;
        const boxes = () => $$('input[name="ids"]', form);
        const deleteButton = $("#delete-selected");
        const countLabel = $("#selected-count");

        const update = () => {
            const selected = boxes().filter((box) => box.checked).length;
            if (deleteButton) deleteButton.disabled = selected === 0;
            if (countLabel) countLabel.textContent = selected + " selected";
        };

        selectAll.addEventListener("change", () => {
            boxes().forEach((box) => {
                box.checked = selectAll.checked;
            });
            update();
        });
        boxes().forEach((box) => box.addEventListener("change", update));
        form.addEventListener("submit", (event) => {
            if (boxes().filter((box) => box.checked).length === 0) {
                event.preventDefault();
                toast("Select at least one file.");
                return;
            }
            const selected = boxes().filter((box) => box.checked).length;
            if (!window.confirm("Delete " + selected + " selected file(s) permanently? This cannot be undone.")) {
                event.preventDefault();
            }
        });

        form.addEventListener("click", (event) => {
            const button = event.target.closest(".delete-one");
            if (!button) return;
            event.preventDefault();
            const id = button.getAttribute("data-id");
            if (!id || !window.confirm("Delete this file permanently? This cannot be undone.")) return;
            const csrf = form.querySelector('input[name="csrf_token"]');
            const submit = document.createElement("form");
            submit.method = "post";
            submit.action = "/admin/files/delete";
            const token = document.createElement("input");
            token.type = "hidden";
            token.name = "csrf_token";
            token.value = csrf ? csrf.value : "";
            const file = document.createElement("input");
            file.type = "hidden";
            file.name = "ids";
            file.value = id;
            submit.append(token, file);
            document.body.appendChild(submit);
            submit.submit();
        });
        update();
    }

    function readUploads() {
        try {
            const raw = window.localStorage.getItem(LOCAL_KEY);
            const parsed = raw ? JSON.parse(raw) : [];
            if (!Array.isArray(parsed)) return [];
            const now = Date.now();
            return parsed.filter((item) => {
                if (!item || typeof item !== "object" || !item.id) return false;
                const expiry = Date.parse(item.expiresAt || "");
                return Number.isNaN(expiry) || expiry > now;
            });
        } catch (err) {
            return [];
        }
    }

    function writeUploads(items) {
        try {
            window.localStorage.setItem(LOCAL_KEY, JSON.stringify(items.slice(0, MAX_LOCAL)));
        } catch (err) {
            toast("Could not save local upload history.");
        }
    }

    function initHome() {
        const app = $("#upload-app");
        if (!app) return;

        const statusURL = app.getAttribute("data-status-url");
        const uploadURL = app.getAttribute("data-upload-url");
        const keyInfoURL = app.getAttribute("data-key-info-url");
        const maxFileSize = Number(app.getAttribute("data-max-file-size")) || 0;
        const restricted = app.getAttribute("data-public-uploads") === "false";
        const input = $("#file-input");
        const dropZone = $("#drop-zone");
        const uploadButton = $("#upload-button");
        const cancelButton = $("#cancel-button");
        const copyButton = $("#copy-links");
        const errorBox = $("#upload-error");
        const queue = $("#upload-queue");
        const fullWarning = $("#storage-full");
        const gate = $("#upload-gate");
        const gateForm = $("#upload-hash-form");
        const gateInput = $("#upload-hash");
        const gateError = $("#upload-hash-error");
        const gateButton = $("#save-upload-hash");
        const authStatus = $("#upload-auth-status");
        const authPrefix = $("#upload-auth-prefix");
        const forgetButton = $("#forget-upload-hash");

        let accepting = app.getAttribute("data-accepting") === "true";
        let uploadKey = null;
        let localUploads = readUploads();
        let tasks = [];
        let active = 0;
        let completed = [];
        let taskID = 0;
        let statusTimer = null;

        writeUploads(localUploads);
        renderLocal();

        function showError(message) {
            if (!errorBox) return;
            errorBox.textContent = message || "";
            errorBox.hidden = !message;
        }

        function showGateError(message) {
            if (!gateError) return;
            gateError.textContent = message || "";
            gateError.hidden = !message;
        }

        function readSavedHash() {
            try {
                const raw = window.localStorage.getItem(UPLOAD_HASH_KEY);
                if (!raw) return null;
                const parsed = JSON.parse(raw);
                if (!parsed || typeof parsed.hash !== "string" || !parsed.hash || parsed.hash.length > 256) return null;
                return parsed;
            } catch (err) {
                return null;
            }
        }

        function saveHash(hash, prefix) {
            try {
                window.localStorage.setItem(UPLOAD_HASH_KEY, JSON.stringify({hash, prefix: prefix || ""}));
            } catch (err) {
                toast("Could not save the user hash in this browser.");
            }
        }

        function clearSavedHash() {
            uploadKey = null;
            try {
                window.localStorage.removeItem(UPLOAD_HASH_KEY);
            } catch (err) {
                // Storage can be unavailable in private modes.
            }
        }

        function updateUploadAvailability() {
            const ready = accepting && (!restricted || Boolean(uploadKey));
            if (uploadButton) uploadButton.disabled = !ready;
            if (dropZone) dropZone.classList.toggle("disabled", !ready);
        }

        function showGate(message) {
            if (!restricted) return;
            clearSavedHash();
            if (gate) gate.hidden = false;
            if (authStatus) authStatus.hidden = true;
            showGateError(message || "");
            updateUploadAvailability();
        }

        function showUnlocked(prefix) {
            if (gate) gate.hidden = true;
            if (authStatus) authStatus.hidden = false;
            if (authPrefix) {
                authPrefix.textContent = prefix ? prefix + "…" : "approved user hash";
            }
            updateUploadAvailability();
        }

        async function validateHash(hash) {
            let response;
            try {
                response = await fetch(keyInfoURL, {
                    headers: {"Authorization": "Bearer " + hash, "Accept": "application/json"},
                    cache: "no-store"
                });
            } catch (err) {
                throw new Error("Could not reach the server. Check your connection and try again.");
            }
            const body = await response.json().catch(() => null);
            if (!response.ok) {
                const message = body && body.error && body.error.message
                    ? body.error.message
                    : "The user hash is invalid or revoked.";
                throw new Error(message);
            }
            if (!body || !body.approved) {
                throw new Error("This user hash is valid but is not approved for uploads.");
            }
            return body;
        }

        function unlockWithHash(hash, prefix, persist) {
            uploadKey = hash;
            if (persist) saveHash(hash, prefix);
            showUnlocked(prefix);
            showGateError("");
            if (gateInput) gateInput.value = "";
            startNext();
        }

        async function initRestrictedUploads() {
            if (!restricted) return;
            const saved = readSavedHash();
            if (!saved) {
                showGate("");
                return;
            }
            if (gate) gate.hidden = true;
            if (authStatus) authStatus.hidden = false;
            if (authPrefix) authPrefix.textContent = (saved.prefix || "saved") + "…";
            try {
                const info = await validateHash(saved.hash);
                unlockWithHash(saved.hash, info.key_prefix || saved.prefix || "", true);
            } catch (err) {
                showGate(err.message || "The saved user hash could not be verified.");
            }
        }

        function setAccepting(next, status) {
            const wasAccepting = accepting;
            accepting = Boolean(next);
            app.setAttribute("data-accepting", accepting ? "true" : "false");
            updateUploadAvailability();
            if (fullWarning) fullWarning.hidden = accepting;
            if (status) {
                const storage = status.storage || {};
                const progress = $("#storage-progress");
                if (progress) progress.value = Number(storage.used_percent) || 0;
                const used = $("#storage-used");
                if (used) used.textContent = formatBytes(storage.used_bytes);
                const limit = $("#storage-limit");
                if (limit) limit.textContent = formatBytes(storage.limit_bytes);
                const percent = $("#storage-percent");
                if (percent) percent.textContent = (Number(storage.used_percent) || 0).toFixed(1);
                const available = $("#storage-available");
                if (available) available.textContent = formatBytes(storage.available_bytes);
                const reserved = $("#storage-reserved");
                if (reserved) {
                    reserved.textContent = Number(storage.reserved_bytes) > 0
                        ? " · " + formatBytes(storage.reserved_bytes) + " reserved by active uploads"
                        : "";
                }
            }
            if (!accepting) {
                showError("Storage maxed out — please try again later.");
            }
            if (accepting && !wasAccepting) startNext();
        }

        async function refreshStatus() {
            if (!statusURL) return;
            try {
                const response = await fetch(statusURL, {headers: {"Accept": "application/json"}, cache: "no-store"});
                if (!response.ok) return;
                const status = await response.json();
                setAccepting(status.accepting_uploads, status);
            } catch (err) {
                // Network hiccups should not disable the UI by themselves.
            }
        }

        function updateCopyButton() {
            if (copyButton) copyButton.disabled = completed.length === 0;
        }

        function renderLocal() {
            const body = $("#local-uploads");
            const empty = $("#local-empty");
            const table = $("#local-table");
            if (!body) return;
            body.textContent = "";
            localUploads.sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt));
            localUploads.forEach((item) => {
                const row = document.createElement("tr");

                const nameCell = document.createElement("td");
                const link = document.createElement("a");
                link.href = item.pageURL || ("/f/" + encodeURIComponent(item.id));
                link.textContent = item.name || item.id;
                nameCell.appendChild(link);

                const sizeCell = document.createElement("td");
                sizeCell.textContent = formatBytes(item.size);

                const expiryCell = document.createElement("td");
                expiryCell.textContent = formatDate(item.expiresAt);

                const actions = document.createElement("td");
                actions.className = "actions";
                const wrap = document.createElement("div");
                wrap.className = "row-actions";

                const copy = document.createElement("button");
                copy.type = "button";
                copy.className = "link-button";
                copy.textContent = "Copy link";
                copy.addEventListener("click", () => copyText(link.href));
                wrap.appendChild(copy);

                const open = document.createElement("a");
                open.href = link.href;
                open.textContent = "Open";
                wrap.appendChild(open);

                if (item.deleteToken) {
                    const del = document.createElement("button");
                    del.type = "button";
                    del.className = "link-button";
                    del.textContent = "Delete";
                    del.addEventListener("click", () => deleteLocal(item));
                    wrap.appendChild(del);
                }

                actions.appendChild(wrap);
                row.append(nameCell, sizeCell, expiryCell, actions);
                body.appendChild(row);
            });
            if (table) table.hidden = localUploads.length === 0;
            if (empty) empty.hidden = localUploads.length > 0;
        }

        async function deleteLocal(item) {
            if (!window.confirm("Delete \"" + (item.name || item.id) + "\" permanently? This cannot be undone.")) return;
            try {
                const response = await fetch("/api/v1/files/" + encodeURIComponent(item.id), {
                    method: "DELETE",
                    headers: {"X-Delete-Token": item.deleteToken}
                });
                if (response.status === 204 || response.status === 404 || response.status === 410) {
                    localUploads = localUploads.filter((entry) => entry.id !== item.id);
                    completed = completed.filter((entry) => entry.id !== item.id);
                    writeUploads(localUploads);
                    renderLocal();
                    updateCopyButton();
                    toast("File deleted");
                    refreshStatus();
                    return;
                }
                let message = "Could not delete the file.";
                try {
                    const body = await response.json();
                    if (body && body.error && body.error.message) message = body.error.message;
                } catch (err) {}
                toast(message);
            } catch (err) {
                toast("Could not reach the server.");
            }
        }

        function addTaskRow(task) {
            const li = document.createElement("li");
            li.className = "upload-item";

            const head = document.createElement("div");
            head.className = "upload-item-head";
            const name = document.createElement("span");
            name.className = "upload-item-name";
            name.textContent = task.file.name;
            const cancel = document.createElement("button");
            cancel.type = "button";
            cancel.className = "link-button";
            cancel.textContent = "Cancel";
            cancel.addEventListener("click", () => abortTask(task));
            head.append(name, cancel);

            const progress = document.createElement("div");
            progress.className = "progress";
            const bar = document.createElement("div");
            bar.className = "progress-bar";
            progress.appendChild(bar);

            const stats = document.createElement("div");
            stats.className = "upload-item-stats";
            const left = document.createElement("span");
            left.textContent = "Queued · " + formatBytes(task.file.size);
            const right = document.createElement("span");
            right.textContent = "0%";
            stats.append(left, right);

            li.append(head, progress, stats);
            if (queue) queue.appendChild(li);
            task.el = {li, bar, left, right, cancel};
        }

        function updateTask(task, text, percent, danger) {
            if (!task.el) return;
            task.el.left.textContent = text;
            task.el.right.textContent = Math.max(0, Math.min(100, Math.round(percent))) + "%";
            task.el.bar.style.width = Math.max(0, Math.min(100, percent)) + "%";
            task.el.bar.classList.toggle("danger", Boolean(danger));
        }

        function enqueue(files) {
            showError("");
            if (restricted && !uploadKey) {
                showGate("Enter your approved user hash before uploading.");
                return;
            }
            if (!accepting) {
                showError("Storage maxed out — please try again later.");
                return;
            }
            let added = 0;
            for (const file of files) {
                if (file.size === 0) {
                    showError("\"" + file.name + "\" is empty.");
                    continue;
                }
                if (maxFileSize > 0 && file.size > maxFileSize) {
                    showError("\"" + file.name + "\" is larger than the " + formatBytes(maxFileSize) + " per-file limit.");
                    continue;
                }
                const task = {id: ++taskID, file, status: "queued", xhr: null, el: null};
                tasks.push(task);
                addTaskRow(task);
                added++;
            }
            if (added > 0 && queue) queue.hidden = false;
            if (added > 0) startNext();
        }

        function startNext() {
            if (!accepting || (restricted && !uploadKey)) return;
            while (active < 2) {
                const task = tasks.find((item) => item.status === "queued");
                if (!task) break;
                startTask(task);
            }
        }

        function startTask(task) {
            if (!accepting) {
                updateTask(task, "Paused · storage full", 0, true);
                return;
            }
            if (restricted && !uploadKey) {
                updateTask(task, "Paused · user hash required", 0, true);
                return;
            }
            task.status = "creating";
            active++;
            if (cancelButton) cancelButton.disabled = false;
            updateTask(task, "Requesting upload URL…", 0, false);

            createUploadIntent(task).then((intent) => {
                if (task.status === "canceled") {
                    abortUploadIntent(intent, false);
                    finishTask(task);
                    return;
                }
                task.intent = intent;
                sendFileDirect(task, intent);
            }).catch((err) => {
                task.status = "failed";
                updateTask(task, err.message || "Could not start upload.", 0, true);
                showError(err.message || "Could not start upload.");
                finishTask(task);
            });
        }

        async function createUploadIntent(task) {
            const headers = {"Content-Type": "application/json", "Accept": "application/json"};
            if (uploadKey) headers.Authorization = "Bearer " + uploadKey;
            const response = await fetch(uploadURL, {
                method: "POST",
                headers,
                body: JSON.stringify({
                    name: task.file.name,
                    size: task.file.size,
                    content_type: task.file.type || "application/octet-stream"
                })
            });
            const body = await response.json().catch(() => null);
            if (!response.ok || !body || !body.url) {
                const code = body && body.error && body.error.code ? body.error.code : "";
                const message = body && body.error && body.error.message
                    ? body.error.message
                    : "Could not create an upload (" + response.status + ").";
                if (restricted && (code === "uploads_restricted" || code === "unauthorized" || code === "upload_not_allowed")) {
                    showGate(message);
                }
                if (response.status === 507) {
                    setAccepting(false);
                    refreshStatus();
                }
                throw new Error(message);
            }
            return body;
        }

        function sendFileDirect(task, intent) {
            if (!intent || !intent.url || !intent.upload_token) {
                task.status = "failed";
                updateTask(task, "Upload URL was incomplete.", 0, true);
                showError("Upload URL was incomplete.");
                finishTask(task);
                return;
            }
            task.status = "uploading";
            const xhr = new XMLHttpRequest();
            task.xhr = xhr;
            xhr.open("PUT", intent.url, true);
            Object.keys(intent.headers || {}).forEach((name) => {
                try {
                    xhr.setRequestHeader(name, intent.headers[name]);
                } catch (err) {
                    // A provider may not require every returned header; if the
                    // browser refuses one, the S3 response will be the source
                    // of truth.
                }
            });
            updateTask(task, "Uploading directly to storage · " + formatBytes(task.file.size), 0, false);

            xhr.upload.addEventListener("progress", (event) => {
                if (!event.lengthComputable) return;
                const percent = event.total > 0 ? (event.loaded / event.total) * 100 : 0;
                updateTask(task, "Uploading " + formatBytes(event.loaded) + " / " + formatBytes(event.total), percent, false);
            });

            xhr.addEventListener("load", () => {
                if (xhr.status >= 200 && xhr.status < 300) {
                    task.status = "completing";
                    updateTask(task, "Verifying upload…", 100, false);
                    completeUploadIntent(intent).then((file) => {
                        task.status = "done";
                        completed.push(file);
                        localUploads.unshift({
                            id: file.id,
                            name: file.name,
                            size: file.size,
                            createdAt: file.created_at,
                            expiresAt: file.expires_at,
                            pageURL: file.page_url,
                            deleteToken: intent.delete_token
                        });
                        localUploads = localUploads.slice(0, MAX_LOCAL);
                        writeUploads(localUploads);
                        renderLocal();
                        updateTask(task, "Done · " + formatBytes(file.size), 100, false);
                        if (task.el) {
                            task.el.cancel.hidden = true;
                            const open = document.createElement("a");
                            open.href = file.page_url;
                            open.textContent = "Open file";
                            task.el.li.appendChild(open);
                        }
                        updateCopyButton();
                        refreshStatus();
                        finishTask(task);
                    }).catch((err) => {
                        task.status = "failed";
                        const message = err.message || "Upload finished, but verification failed.";
                        updateTask(task, message, 100, true);
                        showError(message);
                        finishTask(task);
                    });
                    return;
                }
                task.status = "failed";
                const message = "Upload to storage failed (HTTP " + xhr.status + ").";
                updateTask(task, message, 100, true);
                showError(message);
                abortUploadIntent(intent, false);
                finishTask(task);
            });

            xhr.addEventListener("error", () => {
                task.status = "failed";
                const message = "Upload to storage failed. Check the bucket CORS policy and public endpoint.";
                updateTask(task, message, 0, true);
                showError(message);
                abortUploadIntent(intent, false);
                finishTask(task);
            });

            xhr.addEventListener("abort", () => {
                task.status = "canceled";
                updateTask(task, "Canceled", 0, true);
                abortUploadIntent(intent, false);
                finishTask(task);
            });

            xhr.send(task.file);
        }

        async function completeUploadIntent(intent) {
            let lastError = null;
            for (let attempt = 0; attempt < 3; attempt++) {
                try {
                    const response = await fetch(intent.complete_url, {
                        method: "POST",
                        headers: {"X-Upload-Token": intent.upload_token, "Accept": "application/json"},
                        cache: "no-store"
                    });
                    const body = await response.json().catch(() => null);
                    if (response.ok && body && body.file) return body.file;
                    const message = body && body.error && body.error.message
                        ? body.error.message
                        : "Could not finalize the upload (" + response.status + ").";
                    lastError = new Error(message);
                    if (response.status !== 409 && response.status < 500) break;
                } catch (err) {
                    lastError = err;
                }
                await sleep(1000 + attempt * 1000);
            }
            throw lastError || new Error("Could not finalize the upload.");
        }

        async function abortUploadIntent(intent, keepalive) {
            if (!intent || intent.aborted) return;
            intent.aborted = true;
            try {
                await fetch(intent.abort_url, {
                    method: "DELETE",
                    headers: {"X-Upload-Token": intent.upload_token},
                    keepalive: Boolean(keepalive)
                });
            } catch (err) {
                // The janitor will expire the intent and delete any object.
            }
        }

        function sleep(ms) {
            return new Promise((resolve) => window.setTimeout(resolve, ms));
        }

        function finishTask(task) {
            if (task.finished) return;
            task.finished = true;
            active = Math.max(0, active - 1);
            if (task.el) task.el.cancel.hidden = true;
            if (cancelButton) cancelButton.disabled = !tasks.some((item) => item.status === "creating" || item.status === "uploading" || item.status === "completing" || item.status === "queued");
            startNext();
        }

        function abortTask(task) {
            if (task.status === "queued") {
                task.status = "canceled";
                updateTask(task, "Canceled", 0, true);
                if (task.el) task.el.cancel.hidden = true;
                return;
            }
            if (task.status === "completing") {
                toast("Finalizing upload; please wait.");
                return;
            }
            task.status = "canceled";
            if (task.xhr) {
                task.xhr.abort();
            } else if (task.intent) {
                abortUploadIntent(task.intent, false);
                finishTask(task);
            }
        }

        if (uploadButton) {
            uploadButton.addEventListener("click", () => {
                const files = input && input.files ? Array.from(input.files) : [];
                if (files.length === 0) {
                    showError("Choose at least one file first.");
                    return;
                }
                enqueue(files);
                if (input) input.value = "";
            });
        }

        if (cancelButton) {
            cancelButton.addEventListener("click", () => {
                tasks.forEach((task) => {
                    if (task.status === "creating" || task.status === "uploading" || task.status === "queued") abortTask(task);
                });
                toast("Uploads canceled");
            });
        }

        if (copyButton) {
            copyButton.addEventListener("click", () => {
                if (completed.length === 0) return;
                copyText(completed.map((item) => item.page_url).join("\n"));
            });
        }

        if (gateForm) {
            gateForm.addEventListener("submit", async (event) => {
                event.preventDefault();
                if (gateButton && gateButton.disabled) return;
                const hash = gateInput ? gateInput.value.trim() : "";
                if (!hash) {
                    showGateError("Enter the user hash the admin gave you.");
                    return;
                }
                if (!/^interim_[A-Za-z0-9_-]{8,}$/.test(hash)) {
                    showGateError("That does not look like an interim user hash. It should start with interim_.");
                    return;
                }
                if (gateButton) gateButton.disabled = true;
                showGateError("");
                try {
                    const info = await validateHash(hash);
                    unlockWithHash(hash, info.key_prefix || "", true);
                    toast("Uploads unlocked");
                } catch (err) {
                    showGateError(err.message || "Could not verify that user hash.");
                } finally {
                    if (gateButton) gateButton.disabled = false;
                }
            });
        }

        if (forgetButton) {
            forgetButton.addEventListener("click", () => {
                showGate("");
                toast("User hash forgotten");
            });
        }

        if (dropZone && input) {
            ["dragenter", "dragover"].forEach((name) => {
                dropZone.addEventListener(name, (event) => {
                    event.preventDefault();
                    dropZone.classList.add("dragging");
                });
            });
            ["dragleave", "drop"].forEach((name) => {
                dropZone.addEventListener(name, (event) => {
                    event.preventDefault();
                    dropZone.classList.remove("dragging");
                });
            });
            dropZone.addEventListener("drop", (event) => {
                const transfer = event.dataTransfer;
                if (!transfer || !transfer.files || transfer.files.length === 0) return;
                try {
                    input.files = transfer.files;
                } catch (err) {
                    // Some browsers expose a read-only FileList; fall back to a
                    // direct enqueue of the dropped files.
                    enqueue(Array.from(transfer.files));
                }
            });
        }

        const createHash = $("#create-user-hash");
        if (createHash) {
            createHash.addEventListener("click", async () => {
                createHash.disabled = true;
                try {
                    const response = await fetch("/api/v1/keys", {
                        method: "POST",
                        headers: {"Content-Type": "application/json", "Accept": "application/json"},
                        body: JSON.stringify({name: "browser"})
                    });
                    const body = await response.json();
                    if (!response.ok) {
                        toast(body && body.error ? body.error.message : "Could not create a user hash.");
                        return;
                    }
                    const result = $("#hash-result");
                    const value = $("#hash-value");
                    if (value) value.textContent = body.user_hash;
                    if (result) result.hidden = false;
                    const warning = $("#hash-warning");
                    if (warning) warning.hidden = false;
                    toast("User hash created — copy it now");
                } catch (err) {
                    toast("Could not reach the server.");
                } finally {
                    createHash.disabled = false;
                }
            });
        }

        window.addEventListener("beforeunload", (event) => {
            if (tasks.some((task) => task.status === "creating" || task.status === "uploading" || task.status === "completing" || task.status === "queued")) {
                event.preventDefault();
                event.returnValue = "You have an ongoing upload.";
                return event.returnValue;
            }
        });

        window.addEventListener("pagehide", () => {
            tasks.forEach((task) => {
                if (task.intent && !task.intent.aborted && (task.status === "uploading" || task.status === "completing")) {
                    abortUploadIntent(task.intent, true);
                }
            });
        });

        setAccepting(accepting);
        if (restricted) {
            initRestrictedUploads();
        } else {
            updateUploadAvailability();
        }
        refreshStatus();
        statusTimer = window.setInterval(refreshStatus, 30000);
        window.addEventListener("pagehide", () => {
            if (statusTimer) window.clearInterval(statusTimer);
        });
    }

    addCopyButtons();
    initConfirmForms();
    initDownloadPage();
    initAdmin();
    initHome();
})();
