function usesWindowTheme() {
  return prefs.theme === "document" || prefs.theme === "editor";
}

function initDocumentTheme() {
  $$("[data-window-theme]").forEach(button => {
    button.onclick = () => {
      const theme = button.dataset.windowTheme;
      if (prefs.theme === theme) {
        $("#doc-exit").click();
        return;
      }
      if (!usesWindowTheme()) prefs.previousReadingTheme = prefs.theme;
      prefs.theme = theme;
      $("#settings").classList.remove("show");
      savePrefs();
      applyPrefs();
    };
  });

  $("#doc-exit").onclick = () => {
    prefs.theme = prefs.previousReadingTheme || "light";
    $("#settings").classList.remove("show");
    savePrefs();
    applyPrefs();
  };

  $("#doc-toc").onclick = () => openDrawer(true);
  $("#doc-theme").onclick = () => $("#rail-theme").click();
  $("#doc-fullscreen").onclick = () => {
    $("#settings").classList.remove("show");
    fullscreenButton.click();
  };
  $("#doc-settings").onclick = () => $("#rail-settings").click();
  $("#doc-fullscreen").hidden = fullscreenButton.hidden;
}

function syncDocumentTheme() {
  $$("[data-window-theme]").forEach(button => {
    const active = prefs.theme === button.dataset.windowTheme;
    button.classList.toggle("sel", active);
    button.setAttribute("aria-pressed", String(active));
  });
  const editor = prefs.theme === "editor";
  $("#doc-exit").textContent = editor ? "退出 VS Code 主题" : "退出文档主题";
  $("#doc-theme").hidden = editor;
  $("#doc-theme").textContent = prefs.darkMode ? "日间" : "夜间";
  const root = document.documentElement.style;
  const width = prefs.width === "auto" ? "" : prefs.width + "px";
  root.setProperty("--document-width", width || "min(1170px, 61vw)");
  root.setProperty("--editor-content-width", width || "none");
}
