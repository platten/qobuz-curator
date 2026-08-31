"use strict";

document.addEventListener("DOMContentLoaded", () => {
  const overlay = document.getElementById("wait-overlay");
  const message = document.getElementById("wait-message");
  if (!overlay || !message) return;

  document.querySelectorAll("form[data-wait-message]").forEach((form) => {
    form.addEventListener("submit", () => {
      message.textContent = form.dataset.waitMessage || "Working…";
      overlay.hidden = false;
      document.body.classList.add("waiting");
      window.setTimeout(() => {
        form.querySelectorAll("button[type='submit'], button:not([type])").forEach((button) => {
          button.disabled = true;
        });
      }, 0);
    });
  });

  window.addEventListener("pageshow", () => {
    overlay.hidden = true;
    document.body.classList.remove("waiting");
    document.querySelectorAll("button:disabled").forEach((button) => {
      button.disabled = false;
    });
  });
});
