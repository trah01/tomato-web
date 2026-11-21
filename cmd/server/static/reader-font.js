const READER_FONT_KEY = "fq-reader-font-v1";

function readFontFile(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
}

async function loadReaderFont(data) {
  const comma = data.indexOf(",");
  if (!data.startsWith("data:") || comma < 0 || !data.slice(0, comma).endsWith(";base64")) {
    throw new Error("字体文件格式无效");
  }
  const bytes = Uint8Array.from(atob(data.slice(comma + 1)), c => c.charCodeAt(0));
  return new FontFace("Reader Custom", bytes.buffer).load();
}

async function initReaderFont() {
  const input = document.querySelector("#reader-font-file");
  const upload = document.querySelector("#upload-reader-font");
  const option = document.querySelector("#set-custom-font");
  const status = document.querySelector("#reader-font-status");
  let activeFace;

  function install(face, name) {
    if (activeFace) document.fonts.delete(activeFace);
    document.fonts.add(face);
    activeFace = face;
    option.hidden = false;
    status.textContent = name;
  }

  upload.onclick = () => input.click();
  input.onchange = async () => {
    const [file] = input.files;
    input.value = "";
    if (!file) return;
    if (!/\.(woff2?|ttf|otf)$/i.test(file.name)) {
      status.textContent = "请选择 WOFF2、WOFF、TTF 或 OTF 字体文件";
      return;
    }
    upload.disabled = true;
    status.textContent = "正在加载字体…";
    try {
      const data = await readFontFile(file);
      const face = await loadReaderFont(data);
      localStorage.setItem(READER_FONT_KEY, JSON.stringify({name: file.name, data}));
      install(face, file.name);
      prefs.font = "custom";
      savePrefs();
      applyPrefs();
    } catch (error) {
      status.textContent = error.name === "QuotaExceededError"
        ? "浏览器存储空间不足，请选用更小的字体文件"
        : "无法加载或保存此字体文件";
    } finally {
      upload.disabled = false;
    }
  };

  upload.disabled = true;
  try {
    const saved = JSON.parse(localStorage.getItem(READER_FONT_KEY) || "null");
    if (saved) install(await loadReaderFont(saved.data), saved.name);
  } catch {
    status.textContent = "已保存的字体无法加载，请重新上传";
  } finally {
    if (!activeFace && prefs.font === "custom") {
      prefs.font = "sans";
      applyPrefs();
    }
    upload.disabled = false;
  }
}
