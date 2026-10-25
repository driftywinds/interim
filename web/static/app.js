(function () {
    "use strict";

    const $ = (selector, root) => (root || document).querySelector(selector);
    const $$ = (selector, root) => Array.from((root || document).querySelectorAll(selector));
    const LOCAL_KEY = "interim.uploads.v1";
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
        const maxFileSize = Number(app.getAttribute("data-max-file-size")) || 0;
        const input = $("#file-input");
        const dropZone = $("#drop-zone");
        const uploadButton = $("#upload-button");
        const cancelButton = $("#cancel-button");
        const copyButton = $("#copy-links");
        const errorBox = $("#upload-error");
        const queue = $("#upload-queue");
        const fullWarning = $("#storage-full");

        let accepting = app.getAttribute("data-accepting") === "true";
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

        function setAccepting(next, status) {
            const wasAccepting = accepting;
            accepting = Boolean(next);
            app.setAttribute("data-accepting", accepting ? "true" : "false");
            if (uploadButton) uploadButton.disabled = !accepting;
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
            if (!accepting) return;
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
            task.status = "uploading";
            active++;
            if (cancelButton) cancelButton.disabled = false;
            const xhr = new XMLHttpRequest();
            task.xhr = xhr;
            xhr.open("POST", uploadURL, true);
            xhr.setRequestHeader("X-File-Size", String(task.file.size));
            xhr.setRequestHeader("Accept", "application/json");
            updateTask(task, "Uploading · " + formatBytes(task.file.size), 0, false);

            xhr.upload.addEventListener("progress", (event) => {
                if (!event.lengthComputable) return;
                const percent = event.total > 0 ? (event.loaded / event.total) * 100 : 0;
                updateTask(task, "Uploading " + formatBytes(event.loaded) + " / " + formatBytes(event.total), percent, false);
            });

            xhr.addEventListener("load", () => {
                active--;
                if (xhr.status === 201) {
                    let data = null;
                    try {
                        data = JSON.parse(xhr.responseText);
                    } catch (err) {}
                    if (data && data.id) {
                        task.status = "done";
                        completed.push(data);
                        localUploads.unshift({
                            id: data.id,
                            name: data.name,
                            size: data.size,
                            createdAt: data.created_at,
                            expiresAt: data.expires_at,
                            pageURL: data.page_url,
                            deleteToken: data.delete_token
                        });
                        localUploads = localUploads.slice(0, MAX_LOCAL);
                        writeUploads(localUploads);
                        renderLocal();
                        updateTask(task, "Done · " + formatBytes(data.size), 100, false);
                        if (task.el) {
                            task.el.cancel.hidden = true;
                            const open = document.createElement("a");
                            open.href = data.page_url;
                            open.textContent = "Open file";
                            task.el.li.appendChild(open);
                        }
                        updateCopyButton();
                        refreshStatus();
                    } else {
                        task.status = "failed";
                        updateTask(task, "Done, but the response was unreadable", 100, true);
                    }
                } else {
                    task.status = "failed";
                    let message = "Upload failed (" + xhr.status + ").";
                    try {
                        const body = JSON.parse(xhr.responseText);
                        if (body && body.error && body.error.message) message = body.error.message;
                    } catch (err) {}
                    updateTask(task, message, 100, true);
                    if (xhr.status === 507) {
                        setAccepting(false);
                        refreshStatus();
                    }
                    showError(message);
                }
                finishTask(task);
            });

            xhr.addEventListener("error", () => {
                active--;
                task.status = "failed";
                updateTask(task, "Network error", 0, true);
                finishTask(task);
            });

            xhr.addEventListener("abort", () => {
                active--;
                task.status = "canceled";
                updateTask(task, "Canceled", 0, true);
                finishTask(task);
            });

            xhr.send((() => {
                const data = new FormData();
                data.append("file", task.file, task.file.name);
                return data;
            })());
        }

        function finishTask(task) {
            if (task.el) task.el.cancel.hidden = true;
            if (cancelButton) cancelButton.disabled = !tasks.some((item) => item.status === "uploading" || item.status === "queued");
            startNext();
        }

        function abortTask(task) {
            if (task.status === "queued") {
                task.status = "canceled";
                updateTask(task, "Canceled", 0, true);
                if (task.el) task.el.cancel.hidden = true;
                return;
            }
            if (task.xhr) task.xhr.abort();
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
                    if (task.status === "uploading" || task.status === "queued") abortTask(task);
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
            if (tasks.some((task) => task.status === "uploading" || task.status === "queued")) {
                event.preventDefault();
                event.returnValue = "You have an ongoing upload.";
                return event.returnValue;
            }
        });

        setAccepting(accepting);
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
