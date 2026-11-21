// greylistd-frontend progressive enhancement. Everything works without
// JavaScript (filtering, sorting and actions are plain links and forms); this
// adds live filtering, select-all, a selection counter, confirmations and
// Alt+<letter> keyboard shortcuts in the style of artifactsd.
(function () {
  "use strict";
  document.documentElement.classList.add("js");

  // Alt+<letter> activates the element with a matching data-shortcut.
  document.addEventListener("keydown", function (ev) {
    if (!ev.altKey || ev.ctrlKey || ev.metaKey) return;
    var key = (ev.key || "").toLowerCase();
    if (!/^[a-z]$/.test(key)) return;
    var el = document.querySelector('.nc-content [data-shortcut="' + key + '"]') ||
      document.querySelector('[data-shortcut="' + key + '"]');
    if (!el) return;
    ev.preventDefault();
    if (el.getAttribute("data-shortcut-action") === "focus") {
      el.focus();
      if (el.select) el.select();
    } else if (!el.disabled) {
      el.click();
    }
  });

  // Confirmation for destructive buttons.
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest && ev.target.closest("[data-confirm]");
    if (btn && !window.confirm(btn.getAttribute("data-confirm"))) {
      ev.preventDefault();
    }
  });

  document.addEventListener("DOMContentLoaded", function () {
    var form = document.getElementById("entries-form");
    if (!form) return;
    var rows = Array.prototype.slice.call(form.querySelectorAll("tbody tr[data-filter]"));
    var boxes = rows.map(function (r) { return r.querySelector('input[name="sel"]'); });
    var all = form.querySelector("[data-select-all]");
    var counter = form.querySelector("[data-selected-count]");
    var bulk = Array.prototype.slice.call(form.querySelectorAll("[data-bulk]"));
    var noMatch = form.querySelector(".nc-filter-empty");
    var filter = document.querySelector("[data-live-filter]");

    function visible(r) { return !r.hidden; }

    function update() {
      var n = 0, shown = 0, shownChecked = 0;
      rows.forEach(function (r, i) {
        var on = boxes[i].checked;
        r.classList.toggle("nc-selected", on);
        if (on) n++;
        if (visible(r)) {
          shown++;
          if (on) shownChecked++;
        }
      });
      counter.textContent = n;
      bulk.forEach(function (b) { b.disabled = n === 0; });
      if (all) {
        all.checked = shown > 0 && shownChecked === shown;
        all.indeterminate = shownChecked > 0 && shownChecked < shown;
      }
    }

    form.addEventListener("change", function (ev) {
      if (ev.target === all) {
        rows.forEach(function (r, i) {
          if (visible(r)) boxes[i].checked = all.checked;
        });
      }
      update();
    });

    // Bulk actions only apply to rows that are currently visible.
    form.addEventListener("submit", function (ev) {
      var sub = ev.submitter;
      if (sub && sub.name === "bulk") {
        rows.forEach(function (r, i) {
          if (!visible(r)) boxes[i].checked = false;
        });
      }
    });

    if (filter) {
      // The server already filtered by the q the page was loaded with; a
      // broader filter needs rows it did not send, so reload for that.
      var serverQ = filter.value.trim().toLowerCase();
      var reloadTimer = null;
      var applyFilter = function () {
        var q = filter.value.trim().toLowerCase();
        if (serverQ && q.indexOf(serverQ) === -1) {
          clearTimeout(reloadTimer);
          reloadTimer = setTimeout(function () {
            var u = new URL(window.location.href);
            if (q) u.searchParams.set("q", q); else u.searchParams.delete("q");
            window.location.replace(u.pathname + u.search);
          }, 400);
          return;
        }
        var shown = 0;
        rows.forEach(function (r) {
          var hit = q === "" || r.getAttribute("data-filter").indexOf(q) !== -1;
          r.hidden = !hit;
          if (hit) shown++;
        });
        if (noMatch) noMatch.hidden = shown !== 0 || rows.length === 0;
        // Keep the URL (and the "back" fields used after actions) in sync so
        // reloads and redirects preserve the filter.
        var url = new URL(window.location.href);
        if (q) url.searchParams.set("q", q); else url.searchParams.delete("q");
        var rel = url.pathname + url.search;
        window.history.replaceState(null, "", rel);
        document.querySelectorAll('input[name="back"]').forEach(function (b) { b.value = rel; });
        document.querySelectorAll(".nc-tabs a, .nc-sortbar a, .nc-entries th a").forEach(function (a) {
          var u = new URL(a.href);
          if (q) u.searchParams.set("q", q); else u.searchParams.delete("q");
          a.href = u.pathname + u.search;
        });
        update();
      };
      filter.addEventListener("input", applyFilter);
      // Escape clears the filter.
      filter.addEventListener("keydown", function (ev) {
        if (ev.key === "Escape" && filter.value) {
          filter.value = "";
          applyFilter();
        }
      });
      filter.form.addEventListener("submit", function (ev) { ev.preventDefault(); applyFilter(); });
    }
    update();
  });
})();
