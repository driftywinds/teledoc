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
