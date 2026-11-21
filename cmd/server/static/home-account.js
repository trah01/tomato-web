// 番茄账号面板：首页顶栏「登录」按钮 + 设置面板共用同一套登录流程。
// 凭据存在哪里由站点设置决定（浏览器 localStorage / 服务端），页面只负责带上
// X-FQ-Cookie 请求头，保存由 home.html 里的 api() 统一处理。
(() => {
  const COOKIE_KEY = "fq-fanqie-cookie";
  const state = {loggedIn: false, user: null, mode: "browser"};
  let pollTimer;

  function cancelPolling() {
    clearTimeout(pollTimer);
    pollTimer = undefined;
  }

  async function refresh() {
    try {
      const account = await api("/api/account", {cache: "no-store"});
      state.loggedIn = !!account.loggedIn;
      state.user = account.user || null;
      state.mode = account.mode || "browser";
      // 浏览器里留着的 Cookie 已失效时顺手清掉，避免每次请求都去校验
      if (!state.loggedIn) localStorage.removeItem(COOKIE_KEY);
    } catch {
      // 暂时性的失败不应清掉已知的登录状态
    }
    return state;
  }

  async function logout() {
    await api("/api/account/logout", {method: "POST"});
    localStorage.removeItem(COOKIE_KEY);
    state.loggedIn = false;
    state.user = null;
  }

  // renderStatus 渲染账号状态与退出按钮。
  function renderStatus(host, onDone) {
    host.replaceChildren();
    const status = el("p", {role: "status"});
    const out = el("button", {type: "button", text: "退出登录"});
    host.append(el("p", {text: "已登录：" + ((state.user && state.user.name) || "番茄用户")}), out, status);
    out.onclick = async () => {
      out.disabled = true;
      try {
        await logout();
        onDone && onDone();
      } catch {
        out.disabled = false;
        status.textContent = "退出失败，请重试";
      }
    };
  }

  // renderLogin 渲染扫码 / Cookie 两种登录方式。
  function renderLogin(host, onDone) {
    cancelPolling();
    host.replaceChildren();

    const qrButton = el("button", {type: "button", text: "扫码登录"});
    const qrArea = el("div");
    const qrStatus = el("p", {class: "account-status", role: "status"});
    qrArea.append(qrStatus);
    qrButton.onclick = () => startQR(qrButton, qrArea, qrStatus, onDone);

    const cookie = el("input", {type: "password", autocomplete: "off", "aria-label": "番茄 Cookie", placeholder: "粘贴番茄 Cookie", required: ""});
    const submit = el("button", {type: "submit", text: "用 Cookie 登录"});
    const status = el("div", {role: "status", class: "account-status"});
    const form = el("form", {}, [cookie, submit]);
    form.onsubmit = async event => {
      event.preventDefault();
      if (!cookie.value.trim()) return;
      submit.disabled = true;
      status.textContent = "登录中…";
      try {
        await api("/api/account/cookie", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({cookie: cookie.value.trim()})
        });
        cookie.value = "";
        await refresh();
        onDone && onDone();
      } catch {
        status.textContent = "登录失败，请检查 Cookie 后重试";
        submit.disabled = false;
      }
    };

    host.append(qrButton, qrArea, el("details", {class: "account-cookie"}, [
      el("summary", {text: "使用 Cookie 登录"}), form, status
    ]));
  }

  async function startQR(button, area, status, onDone) {
    button.disabled = true;
    status.textContent = "正在获取二维码…";
    try {
      const qr = await api("/api/account/qr", {method: "POST"});
      if (!qr.image || !qr.token) throw new Error("二维码为空");
      area.querySelector("img")?.remove();
      area.prepend(el("img", {class: "account-qr", src: qr.image, alt: "番茄登录二维码"}));
      status.textContent = "用番茄小说或抖音 App 扫码登录";
      const expiry = Number(qr.expireAt);
      const deadline = expiry > 1e12 ? expiry : expiry > 1e9 ? expiry * 1000 : Date.now() + (expiry > 0 ? expiry : 180) * 1000;
      pollQR(qr.token, deadline, button, status, onDone);
    } catch {
      resetQR(button, status, "获取二维码失败，请重试");
    }
  }

  function resetQR(button, status, message) {
    status.textContent = message;
    button.textContent = "重新获取二维码";
    button.disabled = false;
  }

  function pollQR(token, deadline, button, status, onDone) {
    if (Date.now() >= deadline) {
      resetQR(button, status, "二维码已过期");
      return;
    }
    pollTimer = setTimeout(async () => {
      try {
        const result = await api("/api/account/qr/poll?token=" + encodeURIComponent(token), {cache: "no-store"});
        if (result.state === "success") {
          await refresh();
          onDone && onDone();
          return;
        }
        if (result.state === "error" || result.state === "expired") {
          resetQR(button, status, result.message || "扫码登录失败，请重试");
          return;
        }
        if (result.state === "scanned") status.textContent = "已扫码，请在手机上确认登录";
      } catch {
        status.textContent = "暂时无法检查扫码状态，正在重试…";
      }
      pollQR(token, deadline, button, status, onDone);
    }, 2000);
  }

  /* ---- 首页顶栏账号按钮与账号弹窗 ---- */

  const button = $("#home-account");
  const shelf = $("#home-shelf");
  const historyButton = $("#home-history");
  const dialog = $("#home-account-dialog");
  const title = $("#home-account-title");
  const body = $("#home-account-body");
  let request;

  function cancelView() {
    request?.abort();
    cancelPolling();
  }

  function showView(name) {
    cancelView();
    request = new AbortController();
    title.textContent = name;
    body.replaceChildren();
    if (!dialog.open) dialog.showModal();
  }

  function syncHeader() {
    button.textContent = state.loggedIn ? (state.user && state.user.name) || "我的账号" : "登录";
    button.title = button.textContent;
    shelf.hidden = !state.loggedIn;
    historyButton.hidden = !state.loggedIn;
  }

  function showCollection(path, label) {
    showView(label);
    const signal = request.signal;
    const status = el("p", {role: "status", text: "加载中…"});
    body.append(status);
    api("/api/account/" + path, {signal, cache: "no-store"}).then(result => {
      if (signal.aborted) return;
      const books = result.books || [];
      if (!books.length) {
        status.textContent = path === "shelf" ? "书架还是空的" : "暂无云端阅读记录";
        return;
      }
      body.replaceChildren(el("ul", {class: "account-list"}, books.map(renderBook)));
    }).catch(() => {
      if (signal.aborted) return;
      status.textContent = "加载失败，请重试";
      body.append(el("button", {type: "button", text: "重试", onclick: () => showCollection(path, label)}));
    });
  }

  function renderBook(book) {
    const url = new URLSearchParams({book: book.bookId});
    if (book.lastReadItem && book.lastReadItem !== "0") url.set("chapter", book.lastReadItem);
    const link = el("a", {class: "account-book", href: "/reader?" + url});
    if (book.cover) link.append(el("img", {class: "account-cover", src: book.cover, alt: "", loading: "lazy"}));
    const note = book.chapterTitle ? "读到 " + book.chapterTitle : url.has("chapter") ? "继续阅读" : book.author;
    link.append(el("div", {}, [
      el("strong", {text: book.title || "书籍 " + book.bookId}),
      note && el("span", {class: "account-note", text: note})
    ]));
    return el("li", {}, link);
  }

  dialog.addEventListener("close", () => { if (!dialog.open) cancelView(); });
  button.onclick = async () => {
    await accountReady;
    if (state.loggedIn) {
      showView("我的账号");
      renderStatus(body, () => { syncHeader(); dialog.close(); });
      return;
    }
    showView("登录番茄账号");
    renderLogin(body, () => { syncHeader(); dialog.close(); });
  };
  shelf.onclick = () => showCollection("shelf", "我的书架");
  historyButton.onclick = () => showCollection("history", "阅读历史");

  let accountReady = refresh().then(() => { syncHeader(); });
  window.addEventListener("pageshow", event => {
    if (!event.persisted) return;
    accountReady = refresh().then(() => { syncHeader(); });
  });

  // 设置面板复用同一套登录 / 状态渲染
  window.FQAccount = {state, refresh, logout, renderLogin, renderStatus, syncHeader};
})();
