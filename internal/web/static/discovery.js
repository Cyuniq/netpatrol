function updateCount(form) {
  form.querySelector("[data-selection-count]").textContent =
    form.querySelectorAll('input[name="selected"]:checked').length;
}

document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-pick]");
  if (!button) return;
  const form = button.closest("[data-discovery-selection]");
  for (const checkbox of form.querySelectorAll('input[name="selected"]')) {
    checkbox.checked = button.dataset.pick === "new" ||
      (button.dataset.pick === "both" && checkbox.dataset.both === "true");
  }
  updateCount(form);
});

document.addEventListener("change", (event) => {
  if (event.target.matches('input[name="selected"]')) {
    updateCount(event.target.closest("[data-discovery-selection]"));
  }
});
