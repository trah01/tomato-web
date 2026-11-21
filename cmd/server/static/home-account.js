(() => {
  const button = $("#home-account");
  const shelf = $("#home-shelf");
  const historyButton = $("#home-history");
  const dialog = $("#home-account-dialog");
  const title = $("#home-account-title");
  const body = $("#home-account-body");
  let user = null;
  let request;
  let pollTimer;

  function setUser(next) {
    user = next;
    button.textContent = user ? user.name || "我的账号" : "登录";
    button.title = button.textContent;
    shelf.hidden = !user;
    historyButton.hidden = !user;
  }

  async function refreshAccount() {
    try {
      const account = await api("/api/account", {cache: "no-store"});
      setUser(account.loggedIn ? account.user || {} : null);
    } catch {
      // A temporary failure should not clear a known login state.
    }
  }

  function cancelView() {
    request?.abort();
    clearTimeout(pollTimer);
  }

  function showView(name) {
    cancelView();
    request = new AbortController();
    title.textContent = name;
    body.replaceChildren();
    if (!dialog.open) dialog.showModal();
    return request.signal;
  }

  function showProfile() {
    const signal = showView("我的账号");
    const status = el("p", {role: "status"});
    const logout = el("button", {type: "button", text: "退出登录"});
    body.append(el("p", {text: user.name || "番茄用户"}), logout, status);
    logout.onclick = async () => {
      logout.disabled = true;
      try {
        await api("/api/account/logout", {method: "POST"});
        setUser(null);
        if (!signal.aborted) dialog.close();
      } catch {
        logout.disabled = false;
        status.textContent = "退出失败，请重试";
      }
    };
  }

  function finishLogin(account, signal) {
    setUser(account.user || {});
    if (!signal.aborted) showCollection("shelf", "我的书架");
  }

  function showLogin() {
    const signal = showView("登录番茄账号");
    const qrButton = el("button", {type: "button", text: "获取登录二维码"});
    const qrArea = el("div");
    const qrStatus = el("p", {class: "account-status", role: "status"});
    qrArea.append(qrStatus);
    qrButton.onclick = () => startQR(qrButton, qrArea, qrStatus, signal);

    const cookie = el("input", {type: "password", autocomplete: "off", "aria-label": "番茄 Cookie", placeholder: "粘贴番茄 Cookie", required: ""});
    const submit = el("button", {type: "submit", text: "登录"});
    const status = el("div", {role: "status"});
    const form = el("form", {}, [cookie, submit]);
    form.onsubmit = async event => {
      event.preventDefault();
      if (!cookie.value.trim()) return;
      submit.disabled = true;
      status.textContent = "登录中…";
      try {
        const account = await api("/api/account/cookie", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({cookie: cookie.value.trim()})
        });
        cookie.value = "";
        finishLogin(account, signal);
      } catch {
        status.textContent = "登录失败，请检查 Cookie 后重试";
        submit.disabled = false;
      }
    };
    body.append(qrButton, qrArea, el("details", {class: "account-cookie"}, [
      el("summary", {text: "使用 Cookie 登录"}), form, status
    ]));
  }

  async function startQR(button, area, status, signal) {
    button.disabled = true;
    status.textContent = "正在获取二维码…";
    try {
      const qr = await api("/api/account/qr", {method: "POST", signal});
      if (signal.aborted) return;
      if (!qr.image || !qr.token) throw new Error("二维码为空");
      const previous = area.querySelector("img");
      previous?.remove();
      area.prepend(el("img", {class: "account-qr", src: qr.image, alt: "番茄登录二维码"}));
      status.textContent = "用番茄小说或抖音 App 扫码登录";
      const expiry = Number(qr.expireAt);
      const deadline = expiry > 1e12 ? expiry : expiry > 1e9 ? expiry * 1000 : Date.now() + (expiry > 0 ? expiry : 180) * 1000;
      pollQR(qr.token, deadline, button, status, signal);
    } catch {
      if (!signal.aborted) resetQR(button, status, "获取二维码失败，请重试");
    }
  }

  function resetQR(button, status, message) {
    status.textContent = message;
    button.textContent = "重新获取二维码";
    button.disabled = false;
  }

  function pollQR(token, deadline, button, status, signal) {
    if (signal.aborted) return;
    if (Date.now() >= deadline) {
      resetQR(button, status, "二维码已过期");
      return;
    }
    pollTimer = setTimeout(async () => {
      try {
        const result = await api("/api/account/qr/poll?token=" + encodeURIComponent(token), {signal, cache: "no-store"});
        if (signal.aborted) return;
        if (result.state === "success") {
          finishLogin(result, signal);
          return;
        }
        if (result.state === "error" || result.state === "expired") {
          resetQR(button, status, result.message || "扫码登录失败，请重试");
          return;
        }
        if (result.state === "scanned") status.textContent = "已扫码，请在手机上确认登录";
      } catch {
        if (signal.aborted) return;
        status.textContent = "暂时无法检查扫码状态，正在重试…";
      }
      pollQR(token, deadline, button, status, signal);
    }, 2000);
  }

  async function showCollection(path, label) {
    const signal = showView(label);
    const status = el("p", {role: "status", text: "加载中…"});
    body.append(status);
    try {
      const result = await api("/api/account/" + path, {signal, cache: "no-store"});
      if (signal.aborted) return;
      const books = result.books || [];
      if (!books.length) {
        status.textContent = path === "shelf" ? "书架还是空的" : "暂无云端阅读记录";
        return;
      }
      body.replaceChildren(el("ul", {class: "account-list"}, books.map(renderBook)));
    } catch {
      if (signal.aborted) return;
      status.textContent = "加载失败，请重试";
      body.append(el("button", {type: "button", text: "重试", onclick: () => showCollection(path, label)}));
    }
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

  dialog.addEventListener("close", () => {if (!dialog.open) cancelView();});
  button.onclick = async () => {
    await accountReady;
    if (user) showProfile();
    else showLogin();
  };
  shelf.onclick = () => showCollection("shelf", "我的书架");
  historyButton.onclick = () => showCollection("history", "阅读历史");
  let accountReady = refreshAccount();
  window.addEventListener("pageshow", event => {
    if (event.persisted) accountReady = refreshAccount();
  });
})();
