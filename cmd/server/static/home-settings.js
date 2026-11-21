// 首页独立的「站点设置」面板：访问密码、两步验证、番茄账号绑定方式。
// 这里只负责界面与交互，凭据校验和持久化都在服务端完成。
(() => {
  const button = $("#btn-settings");
  const dialog = $("#home-settings-dialog");
  const body = $("#home-settings-body");

  let settings = null;
  let pending2FA = null; // 已生成但还没确认的两步验证密钥

  async function load() {
    settings = await api("/api/settings", {cache: "no-store"});
    return settings;
  }

  function stateText(on, onLabel, offLabel) {
    return el("span", {class: "set-state", text: on ? onLabel : offLabel});
  }

  function note(kind, notes) {
    const item = notes[kind];
    if (!item) return null;
    return el("p", {class: "set-msg" + (item.error ? " err" : ""), text: item.text});
  }

  /* ---- 访问密码 ---- */

  function accessSection(notes) {
    const sec = el("section", {class: "set-sec"});
    sec.append(el("h3", {}, [
      document.createTextNode("访问密码"),
      stateText(settings.access.enabled, "已开启", "未开启")
    ]));
    sec.append(el("p", {
      class: "set-desc",
      text: "开启后，必须先输入账号密码才能打开首页和阅读内容。密码至少 6 位，大小写和字符类型不限。"
    }));

    const enable = el("input", {type: "checkbox"});
    enable.checked = !!settings.access.enabled;
    const enableLabel = el("label", {class: "set-check"}, [enable, document.createTextNode("需要账号密码才能访问本站")]);

    const username = el("input", {type: "text", autocomplete: "username", placeholder: "访问账号", "aria-label": "访问账号"});
    username.value = settings.access.username || "";
    const password = el("input", {type: "password", autocomplete: "new-password", "aria-label": "访问密码"});
    password.placeholder = settings.access.enabled ? "新密码（留空则不改）" : "访问密码（至少 6 位）";
    const save = el("button", {type: "button", text: "保存"});

    const fields = el("div", {class: "set-field"}, [username, password]);
    const row = el("div", {class: "set-row"}, [save]);

    function syncFields() {
      // 取消勾选后仍要能点保存把密码关掉，只隐藏输入框
      fields.hidden = !enable.checked;
    }
    enable.onchange = syncFields;
    syncFields();

    save.onclick = async () => {
      save.disabled = true;
      const payload = {
        enabled: enable.checked,
        username: username.value.trim(),
        password: password.value
      };
      try {
        const result = await api("/api/settings/access", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify(payload)
        });
        settings = result.settings;
        pending2FA = null;
        render({access: {text: payload.enabled ? "已保存，下次访问需要先登录" : "已关闭访问密码"}});
      } catch (error) {
        render({access: {text: error.message, error: true}});
      }
    };

    sec.append(enableLabel, fields, row);
    const tip = note("access", notes);
    if (tip) sec.append(tip);
    return sec;
  }

  /* ---- 两步验证 ---- */

  function twofaSection(notes) {
    const sec = el("section", {class: "set-sec"});
    sec.append(el("h3", {}, [
      document.createTextNode("两步验证"),
      stateText(settings.twofa.enabled, "已开启", "未开启")
    ]));

    if (!settings.access.enabled) {
      sec.append(el("p", {class: "set-desc", text: "需要先开启访问密码，才能使用两步验证。"}));
      return sec;
    }

    if (settings.twofa.enabled) {
      sec.append(el("p", {
        class: "set-desc",
        text: "登录时需要输入验证器 App（如 Google Authenticator、微软 Authenticator）里的 6 位验证码。关闭时也要输入当前验证码。"
      }));
      const code = el("input", {type: "text", inputmode: "numeric", maxlength: "6", placeholder: "6 位验证码", "aria-label": "两步验证码"});
      const off = el("button", {type: "button", text: "关闭两步验证"});
      off.onclick = async () => {
        off.disabled = true;
        try {
          const result = await api("/api/settings/2fa/disable", {
            method: "POST",
            headers: {"Content-Type": "application/json"},
            body: JSON.stringify({code: code.value.trim()})
          });
          settings = result.settings;
          render({twofa: {text: "已关闭两步验证"}});
        } catch (error) {
          off.disabled = false;
          render({twofa: {text: error.message, error: true}});
        }
      };
      sec.append(el("div", {class: "set-field"}, [code]), el("div", {class: "set-row"}, [off]));
      const tip = note("twofa", notes);
      if (tip) sec.append(tip);
      return sec;
    }

    sec.append(el("p", {
      class: "set-desc",
      text: "开启后，除密码外还要输入验证器 App 生成的 6 位验证码，账号更安全。"
    }));

    if (!pending2FA) {
      const start = el("button", {type: "button", text: "开始设置"});
      start.onclick = async () => {
        start.disabled = true;
        try {
          pending2FA = await api("/api/settings/2fa/start", {method: "POST"});
          render();
        } catch (error) {
          start.disabled = false;
          render({twofa: {text: error.message, error: true}});
        }
      };
      sec.append(el("div", {class: "set-row"}, [start]));
      const tip = note("twofa", notes);
      if (tip) sec.append(tip);
      return sec;
    }

    sec.append(el("p", {
      class: "set-desc",
      text: "在验证器 App 里选择「手动输入密钥」，粘贴下面的密钥并保存；也可以把密钥填入支持 otpauth 链接的 App。"
    }));
    const secretInput = el("input", {class: "set-secret", type: "text", readonly: "", "aria-label": "两步验证密钥"});
    secretInput.value = pending2FA.secret;
    secretInput.onclick = () => secretInput.select();
    const uriInput = el("input", {class: "set-secret", type: "text", readonly: "", "aria-label": "otpauth 链接"});
    uriInput.value = pending2FA.uri;
    uriInput.onclick = () => uriInput.select();

    const code = el("input", {type: "text", inputmode: "numeric", maxlength: "6", placeholder: "App 中显示的 6 位验证码", "aria-label": "两步验证码"});
    const confirm = el("button", {type: "button", text: "确认开启"});
    confirm.onclick = async () => {
      confirm.disabled = true;
      try {
        const result = await api("/api/settings/2fa/enable", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({code: code.value.trim()})
        });
        settings = result.settings;
        pending2FA = null;
        render({twofa: {text: "已开启两步验证，下次登录需要输入验证码"}});
      } catch (error) {
        confirm.disabled = false;
        render({twofa: {text: error.message, error: true}});
      }
    };

    sec.append(secretInput, uriInput, el("div", {class: "set-field"}, [code]), el("div", {class: "set-row"}, [confirm]));
    const tip = note("twofa", notes);
    if (tip) sec.append(tip);
    return sec;
  }

  /* ---- 番茄账号绑定 ---- */

  function fanqieSection(notes) {
    const sec = el("section", {class: "set-sec"});
    sec.append(el("h3", {}, [document.createTextNode("番茄账号")]));
    sec.append(el("p", {
      class: "set-desc",
      text: "这里只决定登录凭据存在哪里。登录和退出在首页右上角的账号按钮里操作，两边不重复。"
    }));

    const modes = [
      ["browser", "仅当前浏览器保存", "登录凭据和 Cookie 只存在这个浏览器里。别人访问本站，或换一个浏览器打开，都是未登录状态。"],
      ["server", "保存到 Docker 容器", "登录凭据和 Cookie 存在服务端数据卷里。任何打开本站的人都会使用这个番茄账号，建议同时开启访问密码。"]
    ];
    modes.forEach(([value, title, desc]) => {
      const field = el("input", {type: "radio", name: "fanqie-mode", value});
      field.checked = settings.fanqie.mode === value;
      const label = el("label", {class: "set-radio"}, [
        field,
        el("div", {}, [el("strong", {text: title}), el("em", {text: desc})])
      ]);
      field.onchange = async () => {
        if (!field.checked) return;
        try {
          const result = await api("/api/settings/fanqie", {
            method: "POST",
            headers: {"Content-Type": "application/json"},
            body: JSON.stringify({mode: value})
          });
          settings = result.settings;
          render({fanqie: {text: value === "browser"
            ? "已切换为仅当前浏览器保存，请到首页右上角重新登录"
            : "已切换为保存到服务端，请到首页右上角重新登录"}});
        } catch (error) {
          render({fanqie: {text: error.message, error: true}});
        }
      };
      sec.append(label);
    });

    const tip = note("fanqie", notes);
    if (tip) sec.append(tip);
    return sec;
  }

  /* ---- 面板 ---- */

  let notes = {};

  function render(nextNotes) {
    notes = nextNotes || {};
    body.replaceChildren(accessSection(notes), twofaSection(notes), fanqieSection(notes));
  }

  async function open() {
    body.replaceChildren(el("p", {role: "status", text: "加载中…"}));
    pending2FA = null;
    if (!dialog.open) dialog.showModal();
    try {
      await load();
      render();
    } catch (error) {
      body.replaceChildren(el("p", {class: "set-msg err", text: "设置加载失败：" + error.message}));
    }
  }

  button.onclick = open;
  dialog.addEventListener("close", () => { pending2FA = null; });
})();
