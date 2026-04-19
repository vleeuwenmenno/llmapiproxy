/* ═══════════════════════════════════════════════════════════════
   shared.js — common utilities for LLM API Proxy web UI
   ═══════════════════════════════════════════════════════════════ */

/* ── Theme ────────────────────────────────────────────────────────────── */
function toggleTheme() {
    var isLight = document.body.classList.toggle("light");
    localStorage.setItem("theme", isLight ? "light" : "dark");
    var icon = isLight ? "\u{1F319}" : "\u2600";
    var themeBtn = document.getElementById("themeBtn");
    if (themeBtn) themeBtn.textContent = icon;
    var m = document.getElementById("themeIconMobile");
    if (m) m.textContent = icon;
}

function applyTheme(theme) {
    if (theme === "system") {
        var isDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
        document.body.classList.toggle("light", !isDark);
    } else {
        document.body.classList.toggle("light", theme === "light");
    }
    var link = document.getElementById("hljs-theme");
    if (link) {
        var isLight = document.body.classList.contains("light");
        link.href = isLight
            ? "https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.9.0/styles/github.min.css"
            : "https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.9.0/styles/github-dark.min.css";
    }
}

function initTheme() {
    var saved = localStorage.getItem("theme") || "dark";
    if (saved === "light") document.body.classList.add("light");
    var themeBtn = document.getElementById("themeBtn");
    if (themeBtn) {
        themeBtn.textContent = document.body.classList.contains("light") ? "\u{1F319}" : "\u2600";
    }
}

/* ── Mobile menu ──────────────────────────────────────────────────────── */
function openMobileMenu() {
    document.getElementById("navLinksMobile").classList.add("open");
    document.getElementById("mobileMenuOverlay").classList.add("open");
    document.body.classList.add("menu-open");
}

function closeMobileMenu() {
    document.getElementById("navLinksMobile").classList.remove("open");
    document.getElementById("mobileMenuOverlay").classList.remove("open");
    document.body.classList.remove("menu-open");
}

/* ── Modal helpers ────────────────────────────────────────────────────── */
function openModal(id) {
    var el = document.getElementById(id);
    if (!el) return;
    el.classList.add("open");
    document.body.classList.add("modal-open");
}

function closeModal(id) {
    var el = document.getElementById(id);
    if (!el) return;
    el.classList.remove("open");
    document.body.classList.remove("modal-open");
}

function closeOnBackdrop(e, id) {
    if (e.target === e.currentTarget) closeModal(id);
}

/* ── Global escape key handler ────────────────────────────────────────── */
document.addEventListener("keydown", function(e) {
    if (e.key === "Escape") {
        closeMobileMenu();
        if (typeof closeBackendDropdown === "function") closeBackendDropdown();
        if (typeof closeModelDropdown === "function") closeModelDropdown();
    }
});

/* ── Cookie helpers ───────────────────────────────────────────────────── */
function setCookie(name, value, days) {
    var expires = "";
    if (days) {
        var d = new Date();
        d.setTime(d.getTime() + days * 24 * 60 * 60 * 1000);
        expires = "; expires=" + d.toUTCString();
    }
    document.cookie = name + "=" + encodeURIComponent(value) + expires + "; path=/; SameSite=Lax";
}

function getCookie(name) {
    var prefix = name + "=";
    var parts = document.cookie ? document.cookie.split("; ") : [];
    for (var i = 0; i < parts.length; i++) {
        if (parts[i].indexOf(prefix) === 0) {
            return decodeURIComponent(parts[i].substring(prefix.length));
        }
    }
    return "";
}

function deleteCookie(name) {
    document.cookie = name + "=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/; SameSite=Lax";
}

/* ── Number/time formatting ───────────────────────────────────────────── */
function fmtDur(ms) {
    if (!ms || ms <= 0) return "0ms";
    if (ms < 1000) return Math.round(ms) + "ms";
    return (ms / 1000).toFixed(2) + "s";
}

function fmtNum(n) {
    if (n == null) return "\u2014";
    if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
    return n.toString();
}

function fmtRelative(ms) {
    var sec = Math.floor(ms / 1000);
    if (sec < 60) return sec + "s ago";
    var min = Math.floor(sec / 60);
    if (min < 60) return min + "m ago";
    var hr = Math.floor(min / 60);
    if (hr < 24) return hr + "h ago";
    var days = Math.floor(hr / 24);
    return days + "d ago";
}

function fmtTime(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    var now = new Date();
    var diffMs = now - d;
    var diffDays = Math.floor(diffMs / 86400000);
    if (diffDays < 90) {
        return (
            '<span title="' +
            d.toLocaleString() +
            '">' +
            fmtRelative(diffMs) +
            "</span>"
        );
    }
    var isToday = d.toDateString() === now.toDateString();
    if (isToday) {
        return d.toLocaleTimeString(undefined, {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
        });
    }
    var pad = function (n) {
        return n < 10 ? "0" + n : n;
    };
    return (
        pad(d.getMonth() + 1) +
        "/" +
        pad(d.getDate()) +
        " " +
        pad(d.getHours()) +
        ":" +
        pad(d.getMinutes())
    );
}

function fmtDateTime(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    return d.toLocaleString(undefined, {
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
    });
}

function fmtCtx(n) {
    if (!n) return "";
    if (n >= 1000000) return Math.round(n / 1000000) + "M";
    if (n >= 1000) return Math.round(n / 1000) + "K";
    return String(n);
}

function formatTokens(n) {
    if (n >= 1000000) return Math.round(n / 1000000) + " M";
    if (n >= 1000) return Math.round(n / 1000) + " K";
    return String(n);
}

/* ── HTML escaping ────────────────────────────────────────────────────── */
function esc(s) {
    return String(s)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;");
}

function escHtml(s) {
    return String(s)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;");
}

/* ── Latency class ────────────────────────────────────────────────────── */
function latClass(ms) {
    if (ms < 800) return "lat-good";
    if (ms < 2000) return "lat-ok";
    return "lat-bad";
}

/* ── Parse time window string ─────────────────────────────────────────── */
function parseWindowMs(w) {
    var m = w.match(/^(\d+)(m|h|d)$/);
    if (!m) return 0;
    var n = parseInt(m[1], 10);
    if (m[2] === "m") return n * 60000;
    if (m[2] === "h") return n * 3600000;
    return n * 86400000;
}

/* ── ISO to local datetime string ─────────────────────────────────────── */
function isoToLocal(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    function pad(n) { return n < 10 ? "0" + n : n; }
    return (
        d.getFullYear() +
        "-" +
        pad(d.getMonth() + 1) +
        "-" +
        pad(d.getDate()) +
        "T" +
        pad(d.getHours()) +
        ":" +
        pad(d.getMinutes())
    );
}

/* ── Clipboard ────────────────────────────────────────────────────────── */
function copyToClipboard(text) {
    return navigator.clipboard.writeText(text);
}

/* ── Model helpers (playground + chat) ────────────────────────────────── */
function modelID(m) {
    return typeof m === "string" ? m : m.id;
}

function modelLabel(m) {
    if (typeof m === "string") return m;
    var label = m.id;
    var tags = [];
    if (m.context_length) tags.push(fmtCtx(m.context_length) + " ctx");
    if (m.max_output_tokens) tags.push(fmtCtx(m.max_output_tokens) + " out");
    return label + (tags.length ? "  (" + tags.join(", ") + ")" : "");
}

function posToTokens(pos) {
    var minLog = Math.log(512);
    var maxLog = Math.log(1000000);
    var val = Math.exp(minLog + (pos / 100) * (maxLog - minLog));
    if (val < 2048) return Math.round(val / 128) * 128;
    if (val < 8192) return Math.round(val / 512) * 512;
    if (val < 65536) return Math.round(val / 4096) * 4096;
    if (val < 524288) return Math.round(val / 16384) * 16384;
    return Math.round(val / 65536) * 65536;
}

/* ── Init on load ─────────────────────────────────────────────────────── */
document.addEventListener("DOMContentLoaded", function() {
    initTheme();
});
