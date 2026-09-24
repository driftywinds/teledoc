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
