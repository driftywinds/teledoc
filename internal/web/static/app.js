// TeleDoc UI glue: keeps pill labels visually in sync with their hidden
// checkboxes (selection feedback for tag pickers and filters).
document.addEventListener('change', function (e) {
  var input = e.target;
  if (input instanceof HTMLInputElement && input.type === 'checkbox' && input.closest('.pill')) {
    input.closest('.pill').classList.toggle('pill-active', input.checked);
  }
});

// Sync initial server-rendered state after load and after htmx swaps.
function syncPills(root) {
  (root || document).querySelectorAll('.pill input[type="checkbox"]').forEach(function (input) {
    input.closest('.pill').classList.toggle('pill-active', input.checked);
  });
}
document.addEventListener('DOMContentLoaded', function () { syncPills(document); });
document.addEventListener('htmx:afterSwap', function (e) { syncPills(e.target); });

// ---------------------------------------------------------------------------
// One-time deletion notice (once per session via sessionStorage).
// Tells the user that deleting a document only removes the DB entry from the
// app — it does NOT delete the file from the Telegram channel.
(function () {
  var KEY = 'teledoc.deleteNoticeShown';
  try {
    if (sessionStorage.getItem(KEY)) return; // already shown this session
    sessionStorage.setItem(KEY, '1');
  } catch (e) {
    return; // storage unavailable — skip the notice
  }
  document.addEventListener('DOMContentLoaded', function () {
    var overlay = document.createElement('div');
    overlay.className = 'notice-backdrop';
    overlay.innerHTML =
      '<div class="notice-card" role="dialog" aria-modal="true" aria-labelledby="notice-title">' +
      '<h2 id="notice-title">About deleting documents</h2>' +
      '<p>Deleting a document here only removes its <strong>entry from this app\'s database</strong>.</p>' +
      '<p class="muted">The original file <strong>stays on your Telegram channel</strong> — this app does not — and cannot — delete files from Telegram.</p>' +
      '<button type="button" class="btn btn-primary" id="notice-ok">Got it</button>' +
      '</div>';
    overlay.addEventListener('click', function (e) {
      if (e.target === overlay) dismiss();
    });
    var ok = overlay.querySelector('#notice-ok');
    if (ok) ok.addEventListener('click', dismiss);
    function dismiss() {
      if (overlay && overlay.parentNode) overlay.parentNode.removeChild(overlay);
    }
    document.body.appendChild(overlay);
  });
})();

// Confirm before actually deleting a document row.
document.addEventListener('submit', function (e) {
  var form = e.target;
  if (form && form.hasAttribute('confirm-doc-delete')) {
    if (!window.confirm('Remove this document from the archive? The file on Telegram will NOT be deleted.')) {
      e.preventDefault();
    }
  }
});

// Documents table: keep the "select all" checkbox (in the header's checkbox
// column), the bulk-delete toolbar's visibility, and its "N selected" count
// all in sync with the individual per-row checkboxes.
function syncBulkToolbar(root) {
  var scope = root || document;
  var table = scope.querySelector('.doc-table');
  if (!table) return;
  var rows = table.querySelectorAll('tbody .row-select');
  var checked = Array.prototype.filter.call(rows, function (cb) { return cb.checked; });

  var selectAll = table.querySelector('thead [data-select-all]');
  if (selectAll) {
    selectAll.checked = rows.length > 0 && checked.length === rows.length;
    selectAll.indeterminate = checked.length > 0 && checked.length < rows.length;
  }

  var toolbar = document.getElementById('bulk-toolbar');
  var count = document.getElementById('bulk-toolbar-count');
  if (toolbar) toolbar.hidden = checked.length === 0;
  if (count) count.textContent = checked.length + ' selected';
}

document.addEventListener('change', function (e) {
  var el = e.target;
  if (!(el instanceof HTMLInputElement) || el.type !== 'checkbox') return;
  var table = el.closest('.doc-table');
  if (!table) return;
  var rows = table.querySelectorAll('tbody .row-select');
  if (el.hasAttribute('data-select-all')) {
    rows.forEach(function (cb) { cb.checked = el.checked; });
  }
  syncBulkToolbar(document);
});

document.addEventListener('DOMContentLoaded', function () { syncBulkToolbar(document); });
document.addEventListener('htmx:afterSwap', function () { syncBulkToolbar(document); });

// Confirm before bulk-deleting the selected documents.
document.addEventListener('submit', function (e) {
  var form = e.target;
  if (form && form.hasAttribute('confirm-bulk-delete')) {
    var n = document.querySelectorAll('.doc-table tbody .row-select:checked').length;
    var msg = 'Remove ' + n + (n === 1 ? ' document' : ' documents') +
      ' from the archive? The file(s) on Telegram will NOT be deleted.';
    if (n === 0 || !window.confirm(msg)) {
      e.preventDefault();
    }
  }
});

// When the per-page <select> changes, send an htmx GET with all filter params
// plus the new per_page value.  htmx's hx-include="#filter-form" takes care of
// the search/filter parameters; the select's own name/value provides per_page.
// We also auto-submit on change via htmx native support, but we need to ensure
// the query params include page=1 for a fresh result.
document.addEventListener('htmx:configRequest', function (e) {
  // If per_page select triggered this request, remove the "page" param so we start at page 1
  if (e.detail.elt && e.detail.elt.name === 'per_page') {
    // htmx will include the select's value; we just need to ensure no stale page param
  }
});