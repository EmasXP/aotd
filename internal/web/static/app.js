// Small behaviours that would otherwise need inline handlers, which the CSP
// forbids.

// Missing cover art: drop the <img> so the coloured placeholder shows.
document.addEventListener("error", (e) => {
  const t = e.target;
  if (t instanceof HTMLImageElement && t.closest(".cover")) t.remove();
}, true);

// Enter in the album search shouldn't submit the post form.
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target instanceof HTMLElement && e.target.matches("[data-no-submit]")) {
    e.preventDefault();
  }
});

// Close the account menu when clicking elsewhere.
document.addEventListener("click", (e) => {
  document.querySelectorAll("details.menu[open]").forEach((d) => {
    if (!d.contains(e.target)) d.removeAttribute("open");
  });
});

// Server errors aren't swapped in; tell the user instead of failing silently.
document.addEventListener("htmx:responseError", (e) => {
  if (e.detail.xhr.status >= 500) alert("Something went wrong. Please try again.");
});
