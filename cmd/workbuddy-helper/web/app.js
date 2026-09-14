(() => {
  "use strict";

  const appToken = String(
    window.__APP_TOKEN__ || document.querySelector('meta[name="app-token"]')?.getAttribute("content") || "",
  );

  const elements = {
    connectionStatus: document.querySelector("#connection-status"),
    connectionLabel: document.querySelector("#connection-label"),
    lastUpdated: document.querySelector("#last-updated"),
    summaryTotal: document.querySelector("#summary-total"),
    summaryChecked: document.querySelector("#summary-checked"),
    summaryCheckedLabel: document.querySelector("#summary-checked-label"),
    summaryPoints: document.querySelector("#summary-points"),
    summaryEnabled: document.querySelector("#summary-enabled"),
    accountCount: document.querySelector("#account-count"),
    accountRows: document.querySelector("#account-rows"),
    tableState: document.querySelector("#table-state"),
    search: document.querySelector("#account-search"),
    statusFilter: document.querySelector("#status-filter"),
    refreshState: document.querySelector("#refresh-state"),
    runAll: document.querySelector("#run-all"),
    runAllLabel: document.querySelector("#run-all-label"),
    refreshPointsAll: document.querySelector("#refresh-points-all"),
    addAccount: document.querySelector("#add-account"),
    scheduleForm: document.querySelector("#schedule-form"),
    scheduleEnabled: document.querySelector("#schedule-enabled"),
    scheduleTime: document.querySelector("#schedule-time"),
    scheduleSummary: document.querySelector("#schedule-summary"),
    saveSchedule: document.querySelector("#save-schedule"),
    versionMode: document.querySelector("#version-mode"),
    modeHint: document.querySelector("#mode-hint"),
    versionSwitch: document.querySelector("#version-switch"),
    proxyScheme: document.querySelector("#proxy-scheme"),
    proxyHost: document.querySelector("#proxy-host"),
    pageTitle: document.querySelector("#page-title"),
    activeModel: document.querySelector("#active-model"),
    activeModelRow: document.querySelector("#active-model-row"),
    recheckDelay: document.querySelector("#recheck-delay"),
    recheckDelayRow: document.querySelector("#recheck-delay-row"),
    colDaily: document.querySelector("#col-daily"),
    drawer: document.querySelector("#log-drawer"),
    drawerBackdrop: document.querySelector("#drawer-backdrop"),
    openLogs: document.querySelector("#open-logs"),
    closeLogs: document.querySelector("#close-logs"),
    refreshLogs: document.querySelector("#refresh-logs"),
    logList: document.querySelector("#log-list"),
    logSubtitle: document.querySelector("#log-subtitle"),
    loginDialog: document.querySelector("#login-dialog"),
    loginForm: document.querySelector("#login-form"),
    loginStarting: document.querySelector("#login-starting"),
    loginReady: document.querySelector("#login-ready"),
    loginError: document.querySelector("#login-error"),
    loginErrorMessage: document.querySelector("#login-error-message"),
    loginUrlLabel: document.querySelector("#login-url-label"),
    openLoginLink: document.querySelector("#open-login-link"),
    copyLoginLink: document.querySelector("#copy-login-link"),
    loginPollStatus: document.querySelector("#login-poll-status"),
    retryLogin: document.querySelector("#retry-login"),
    editDialog: document.querySelector("#edit-dialog"),
    editForm: document.querySelector("#edit-form"),
    editAccountId: document.querySelector("#edit-account-id"),
    nicknameInput: document.querySelector("#nickname-input"),
    closeEdit: document.querySelector("#close-edit"),
    cancelEdit: document.querySelector("#cancel-edit"),
    saveNickname: document.querySelector("#save-nickname"),
    toastRegion: document.querySelector("#toast-region"),
  };

  const app = {
    accounts: [],
    allAccounts: [],
    viewVersion: "cn",
    settings: {
      scheduleEnabled: false,
      scheduleTime: "09:15",
      mode: "cn",
      activeModel: "hy3",
      recheckDelayMinutes: 60,
      proxyUrl: "",
    },
    isLoading: true,
    loadError: "",
    busyAccounts: new Set(),
    loginId: "",
    loginUrl: "",
    loginPollTimer: null,
    loginPollGeneration: 0,
    editingAccountId: "",
  };

  const numberFormatter = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });
  const integerFormatter = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 0 });
  const dateTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: "Asia/Shanghai",
  });
  const timeFormatter = new Intl.DateTimeFormat("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
    timeZone: "Asia/Shanghai",
  });

  function refreshIcons(root = document) {
    if (window.lucide && typeof window.lucide.createIcons === "function") {
      window.lucide.createIcons({
        attrs: { "aria-hidden": "true" },
        root,
      });
    }
  }

  function escapeHTML(value) {
    return String(value ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#039;");
  }

  function getErrorMessage(error, fallback = "操作失败，请稍后重试") {
    if (error instanceof Error && error.message) return error.message;
    if (typeof error === "string" && error.trim()) return error;
    return fallback;
  }

  async function api(path, options = {}) {
    const method = String(options.method || "GET").toUpperCase();
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");

    if (method !== "GET" && method !== "HEAD") {
      headers.set("X-App-Token", appToken);
    }
    if (options.body !== undefined && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }

    const response = await fetch(path, { ...options, method, headers });
    const contentType = response.headers.get("content-type") || "";
    let payload = null;

    if (response.status !== 204) {
      if (contentType.includes("application/json")) {
        payload = await response.json().catch(() => null);
      } else {
        const text = await response.text();
        payload = text ? { raw: text } : null;
      }
    }

    if (!response.ok) {
      const message =
        payload?.message ||
        payload?.error?.message ||
        (typeof payload?.error === "string" ? payload.error : "") ||
        payload?.raw ||
        `请求失败 (${response.status})`;
      const error = new Error(message);
      error.status = response.status;
      error.payload = payload;
      throw error;
    }

    return payload ?? {};
  }

  function firstDefined(...values) {
    return values.find((value) => value !== undefined && value !== null && value !== "");
  }

  function parseBoolean(value, fallback = false) {
    if (typeof value === "boolean") return value;
    if (typeof value === "number") return value !== 0;
    if (typeof value === "string") {
      if (["true", "1", "yes", "enabled", "active"].includes(value.toLowerCase())) return true;
      if (["false", "0", "no", "disabled", "inactive"].includes(value.toLowerCase())) return false;
    }
    return fallback;
  }

  function parseNumber(value) {
    if (typeof value === "number" && Number.isFinite(value)) return value;
    if (typeof value === "string" && value.trim()) {
      const parsed = Number(value.replaceAll(",", ""));
      if (Number.isFinite(parsed)) return parsed;
    }
    return null;
  }

  function normalizeTime(value) {
    if (typeof value !== "string") return "09:15";
    const match = value.match(/^(\d{1,2}):(\d{2})/);
    if (!match) return "09:15";
    const hour = Math.min(23, Math.max(0, Number(match[1])));
    const minute = Math.min(59, Math.max(0, Number(match[2])));
    return `${String(hour).padStart(2, "0")}:${String(minute).padStart(2, "0")}`;
  }

  function normalizeAccount(raw, index) {
    const id = String(firstDefined(raw?.id, raw?.uid, raw?.accountId, raw?.userId, index));
    const displayName = String(firstDefined(raw?.displayName, raw?.alias, raw?.nickname, raw?.name, ""));
    const nickname = String(firstDefined(raw?.alias, raw?.displayName, raw?.nickname, raw?.name, ""));
    const identity = String(
      firstDefined(raw?.email, raw?.phone, raw?.username, raw?.uid, raw?.userId, raw?.id, `账户 ${index + 1}`),
    );
    const enabled = raw?.enabled !== undefined ? parseBoolean(raw.enabled, true) : !parseBoolean(raw?.disabled, false);
    const points = parseNumber(
      firstDefined(
        raw?.points,
        raw?.credits,
      raw?.credit,
      raw?.balance,
        raw?.score,
        raw?.quota?.points,
        raw?.point?.total,
      ),
    );
    const checkinRaw = firstDefined(
      raw?.checkinStatus,
      raw?.checkInStatus,
      raw?.signinStatus,
      raw?.signInStatus,
      raw?.lastCheckin,
      raw?.today?.status,
      raw?.checkin?.status,
    );
    const checkedFlag = firstDefined(
      raw?.checkedInToday,
      raw?.checkedIn,
      raw?.signedToday,
      raw?.isCheckedIn,
      raw?.today?.checked,
      raw?.checkin?.checked,
    );
    const healthRaw = String(firstDefined(raw?.status, raw?.health, raw?.tokenStatus, "")).toLowerCase();
    const reason = String(firstDefined(raw?.reason, raw?.lastError, raw?.error, raw?.message, raw?.statusMessage, ""));

    return {
      ...raw,
      id,
      version: String(firstDefined(raw?.version, raw?.region === "global" ? "intl" : "cn", "cn")),
      displayName,
      nickname,
      identity,
      enabled,
      points,
      checkinRaw,
      checkedFlag: checkedFlag === undefined ? inferCheckedToday(raw, checkinRaw) : checkedFlag,
      healthRaw,
      reason,
      updatedAt: firstDefined(
        raw?.pointsUpdatedAt,
        raw?.creditUpdatedAt,
        raw?.lastPointsAt,
        raw?.updatedAt,
        raw?.lastUpdated,
      ),
    };
  }

  function localDateKey(date = new Date()) {
    const parts = new Intl.DateTimeFormat("en-CA", {
      timeZone: "Asia/Shanghai",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).formatToParts(date);
    const values = Object.fromEntries(parts.map((part) => [part.type, part.value]));
    return `${values.year}-${values.month}-${values.day}`;
  }

  function inferCheckedToday(raw, checkinRaw) {
    const state = String(checkinRaw ?? "").toLowerCase();
    if (!["confirmed", "already_done", "success", "done", "checked", "checked_in", "signed", "signed_in", "ok", "completed"].includes(state)) {
      return undefined;
    }
    const dateValue = firstDefined(raw?.lastCheckinDate, raw?.checkinDate, raw?.today?.date);
    if (!dateValue) return ["confirmed", "already_done"].includes(state);
    const dateText = String(dateValue).slice(0, 10);
    return dateText === localDateKey();
  }

  function normalizeState(payload) {
    const source = payload?.data && typeof payload.data === "object" ? payload.data : payload || {};
    const rawAccounts = Array.isArray(source) ? source : source.accounts || source.items || [];
    const rawSettings = source.settings || source.schedule || payload?.settings || {};

    const cnList = Array.isArray(source.cnAccounts) ? source.cnAccounts.map(normalizeAccount) : null;
    const intlList = Array.isArray(source.intlAccounts) ? source.intlAccounts.map(normalizeAccount) : null;
    const fallback = Array.isArray(rawAccounts) ? rawAccounts.map(normalizeAccount) : [];
    return {
      // 两套账号列表：国内版 / 国际版独立管理，按当前查看版本取对应列表
      cnAccounts: cnList || fallback.filter((a) => (a.version || "cn") !== "intl"),
      intlAccounts: intlList || fallback.filter((a) => a.version === "intl"),
      settings: {
        scheduleEnabled: parseBoolean(
          firstDefined(rawSettings.scheduleEnabled, rawSettings.enabled, source.scheduleEnabled),
          false,
        ),
        scheduleTime: normalizeTime(firstDefined(rawSettings.scheduleTime, rawSettings.time, source.scheduleTime, "09:15")),
        mode: String(firstDefined(rawSettings.mode, source.mode, "cn")) === "intl" ? "intl" : "cn",
        activeModel: ["hy3", "hy4-preview"].includes(String(rawSettings.activeModel))
          ? String(rawSettings.activeModel)
          : "hy3",
        recheckDelayMinutes: clampRecheckDelay(parseNumber(rawSettings.recheckDelayMinutes) ?? 60),
        proxyUrl: String(firstDefined(rawSettings.proxyUrl, source.proxyUrl, "") || ""),
      },
    };
  }

  // 代理地址拆分：socks5://user:pass@host:port -> { scheme, host, auth }
  function parseProxyURL(value) {
    const raw = String(value || "").trim();
    if (!raw) return { scheme: "", host: "" };
    const m = raw.match(/^(https?|socks5h?|):\/\/(.+)$/i);
    if (!m) return { scheme: "", host: raw };
    return { scheme: m[1].toLowerCase(), host: m[2] };
  }

  function joinProxyURL(scheme, host) {
    scheme = String(scheme || "").trim().toLowerCase();
    host = String(host || "").trim();
    if (!host) return "";
    return scheme ? `${scheme}://${host}` : host;
  }

  function clampRecheckDelay(value) {
    if (!Number.isFinite(value)) return 60;
    return Math.min(720, Math.max(5, Math.round(value)));
  }

  function getCheckinState(account) {
    if (typeof account.checkedFlag === "boolean") {
      return account.checkedFlag
        ? { key: "done", label: "已签到", className: "checkin-done" }
        : { key: "pending", label: "未签到", className: "checkin-pending" };
    }

    const value = String(account.checkinRaw ?? "").toLowerCase();
    if (["confirmed", "already_done", "success", "done", "checked", "checked_in", "signed", "signed_in", "ok", "completed"].includes(value)) {
      return { key: "done", label: "已签到", className: "checkin-done" };
    }
    if (["failed", "error", "expired"].includes(value)) {
      return { key: "error", label: "签到失败", className: "checkin-error" };
    }
    if (["pending", "not_checked", "unsigned", "not_signed", "ready"].includes(value)) {
      return { key: "pending", label: "未签到", className: "checkin-pending" };
    }
    return { key: "unknown", label: "暂无记录", className: "checkin-unknown" };
  }

  // 国际版活跃状态：LastActive confirmed 且日期为今天 → 已活跃
  function getActiveState(account) {
    const state = String(account.lastActive ?? "").toLowerCase();
    const isToday = account.lastActiveDate === localDateKey();
    if (isToday && state === "confirmed") {
      return { key: "done", label: "已活跃", className: "checkin-done" };
    }
    if (state === "confirmed" && !isToday) {
      return { key: "pending", label: "未活跃", className: "checkin-pending" };
    }
    if (["failed", "error"].includes(state)) {
      return { key: "error", label: "活跃失败", className: "checkin-error" };
    }
    return { key: "pending", label: "未活跃", className: "checkin-pending" };
  }

  function currentMode() {
    return app.settings.mode === "intl" ? "intl" : "cn";
  }

  // 今日任务状态：国内版列表取签到，国际版列表取活跃（跟随当前查看的版本）
  function getDailyState(account) {
    return app.viewVersion === "intl" ? getActiveState(account) : getCheckinState(account);
  }

  // 积分复核倒计时提示（国际版活跃后积分延迟到账）
  function formatRecheckMeta(account) {
    if (!account.pendingRecheckAt) return "";
    const due = new Date(account.pendingRecheckAt);
    if (Number.isNaN(due.getTime())) return "";
    const attempts = Number(account.recheckAttempts) || 0;
    const suffix = attempts > 0 ? ` · 第 ${attempts + 1} 次复核` : "";
    return `等待积分到账 ${timeFormatter.format(due)}${suffix}`;
  }

  function getHealthState(account) {
    if (!account.enabled) return { key: "disabled", label: "已停用", className: "status-muted" };
    if (account.cooling || ["cooling", "rate_limited", "limited"].includes(account.healthRaw)) {
      return { key: "attention", label: "冷却中", className: "status-warn" };
    }
    if (["needs_login", "unauthorized", "expired", "auth_dead"].includes(account.healthRaw)) {
      return { key: "attention", label: "登录失效", className: "status-error" };
    }
    if (["running", "pending"].includes(account.healthRaw)) {
      return { key: "attention", label: "执行中", className: "status-warn" };
    }
    if (["ready", "confirmed", "already_done", "ok", "success", "done"].includes(account.healthRaw)) {
      return { key: "enabled", label: "正常", className: "status-ok" };
    }
    if (
      account.reason ||
      parseBoolean(account.expired, false) ||
      ["error", "failed", "expired", "invalid", "unauthorized"].includes(account.healthRaw)
    ) {
      return { key: "attention", label: account.healthRaw === "expired" ? "登录失效" : "需处理", className: "status-error" };
    }
    return { key: "enabled", label: "正常", className: "status-ok" };
  }

  function accountDisplayName(account) {
    return account.displayName || account.nickname || account.identity || `账户 ${account.id}`;
  }

  function accountInitial(account) {
    const value = accountDisplayName(account).trim();
    if (!value) return "W";
    return Array.from(value)[0].toUpperCase();
  }

  function formatPoints(value) {
    return value === null ? "--" : numberFormatter.format(value);
  }

  function formatUpdatedAt(value) {
    if (!value) return "尚未查询";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return `${dateTimeFormatter.format(date)} 更新`;
  }

  function renderSummary() {
    const intl = app.viewVersion === "intl";
    const enabled = app.accounts.filter((account) => account.enabled).length;
    const checked = app.accounts.filter((account) => getDailyState(account).key === "done").length;
    const pointsValues = app.accounts.map((account) => account.points).filter((value) => value !== null);
    const pointsTotal = pointsValues.reduce((sum, value) => sum + value, 0);

    elements.summaryTotal.textContent = integerFormatter.format(app.accounts.length);
    elements.summaryChecked.textContent = integerFormatter.format(checked);
    elements.summaryCheckedLabel.textContent = intl ? "今日已活跃" : "今日已签到";
    elements.summaryEnabled.textContent = integerFormatter.format(enabled);
    elements.summaryPoints.textContent = pointsValues.length ? numberFormatter.format(pointsTotal) : "--";
  }

  function accountMatchesFilter(account) {
    const query = elements.search.value.trim().toLocaleLowerCase("zh-CN");
    const filter = elements.statusFilter.value;
    const health = getHealthState(account);
    const haystack = `${account.nickname} ${account.identity} ${account.id}`.toLocaleLowerCase("zh-CN");

    if (query && !haystack.includes(query)) return false;
    if (filter === "all") return true;
    if (filter === "enabled") return account.enabled;
    if (filter === "disabled") return !account.enabled;
    if (filter === "attention") return health.key === "attention";
    return true;
  }

  function renderTableState(kind, title, detail, actionLabel = "") {
    const icon = kind === "error" ? "wifi-off" : kind === "empty" ? "users" : kind === "search" ? "search-x" : "loader-circle";
    const action = actionLabel
      ? `<button class="button button-secondary" type="button" data-state-action="${kind}">
          <i data-lucide="${kind === "error" ? "rotate-cw" : "user-plus"}" aria-hidden="true"></i>
          <span>${escapeHTML(actionLabel)}</span>
        </button>`
      : "";
    elements.tableState.innerHTML = `
      ${kind === "loading" ? '<div class="loading-spinner" aria-hidden="true"></div>' : `<div class="table-state-icon"><i data-lucide="${icon}" aria-hidden="true"></i></div>`}
      <strong>${escapeHTML(title)}</strong>
      <span>${escapeHTML(detail)}</span>
      ${action}
    `;
    elements.tableState.hidden = false;
    refreshIcons(elements.tableState);
  }

  function renderAccounts() {
    if (app.isLoading) {
      elements.accountRows.innerHTML = "";
      renderTableState("loading", "正在读取账户", "请稍候");
      elements.accountCount.textContent = "正在同步";
      return;
    }

    if (app.loadError && !app.accounts.length) {
      elements.accountRows.innerHTML = "";
      renderTableState("error", "无法读取账户", app.loadError, "重新加载");
      elements.accountCount.textContent = "连接失败";
      return;
    }

    const filtered = app.accounts.filter(accountMatchesFilter);
    elements.accountCount.textContent = filtered.length === app.accounts.length
      ? `${integerFormatter.format(app.accounts.length)} 个账户`
      : `显示 ${integerFormatter.format(filtered.length)} / ${integerFormatter.format(app.accounts.length)}`;

    if (!app.accounts.length) {
      elements.accountRows.innerHTML = "";
      renderTableState("empty", "还没有账户", "添加账户后即可签到并查询积分", "添加账户");
      return;
    }

    if (!filtered.length) {
      elements.accountRows.innerHTML = "";
      renderTableState("search", "没有匹配的账户", "调整搜索内容或筛选条件");
      return;
    }

    elements.tableState.hidden = true;
    const intl = app.viewVersion === "intl";
    elements.accountRows.innerHTML = filtered
      .map((account) => {
        const daily = getDailyState(account);
        const health = getHealthState(account);
        const busy = app.busyAccounts.has(account.id);
        const name = accountDisplayName(account);
        const detail = account.nickname && account.identity !== account.nickname ? account.identity : `ID: ${account.id}`;
        const reasonTooltip = account.reason ? ` data-tooltip="${escapeHTML(account.reason)}"` : "";
        const recheckMeta = formatRecheckMeta(account);
        const pointsMeta = recheckMeta || formatUpdatedAt(account.updatedAt);
        const dailyActionIcon = intl ? "send-horizontal" : "calendar-check-2";
        const dailyActionLabel = intl ? "活跃" : "签到";

        return `
          <tr class="${account.enabled ? "" : "is-disabled"} ${busy ? "row-busy" : ""}" data-account-id="${escapeHTML(account.id)}">
            <td data-label="账户">
              <div class="account-cell">
                <span class="account-avatar" aria-hidden="true">${escapeHTML(accountInitial(account))}</span>
                <span class="account-identity">
                  <span class="account-name" title="${escapeHTML(name)}">${escapeHTML(name)}</span>
                  <span class="account-detail" title="${escapeHTML(detail)}">${escapeHTML(detail)}</span>
                </span>
              </div>
            </td>
            <td data-label="状态">
              <span class="status-badge ${health.className}"${reasonTooltip}>${escapeHTML(health.label)}</span>
            </td>
            <td data-label="${intl ? "今日活跃" : "今日签到"}">
              <span class="checkin-label ${daily.className}">${escapeHTML(daily.label)}</span>
            </td>
            <td data-label="积分">
              <strong class="points-value">${escapeHTML(formatPoints(account.points))}</strong>
              <span class="points-meta">${escapeHTML(pointsMeta)}</span>
            </td>
            <td data-label="启用">
              <label class="switch" data-tooltip="${account.enabled ? "停用账户" : "启用账户"}">
                <input type="checkbox" data-action="toggle" aria-label="${account.enabled ? "停用" : "启用"}${escapeHTML(name)}" ${account.enabled ? "checked" : ""} ${busy ? "disabled" : ""} />
                <span aria-hidden="true"></span>
              </label>
            </td>
            <td data-label="操作">
              <div class="row-actions">
                <button class="icon-button" type="button" data-action="${intl ? "active" : "checkin"}" aria-label="为${escapeHTML(name)}${intl ? "发送活跃会话" : "签到"}" data-tooltip="${dailyActionLabel}" ${busy || !account.enabled ? "disabled" : ""}>
                  <i data-lucide="${dailyActionIcon}" aria-hidden="true"></i>
                </button>
                <button class="icon-button" type="button" data-action="points" aria-label="查询${escapeHTML(name)}积分" data-tooltip="查询积分" ${busy || !account.enabled ? "disabled" : ""}>
                  <i data-lucide="refresh-cw" aria-hidden="true"></i>
                </button>
                <button class="icon-button" type="button" data-action="edit" aria-label="修改${escapeHTML(name)}名称" data-tooltip="修改名称" ${busy ? "disabled" : ""}>
                  <i data-lucide="pencil" aria-hidden="true"></i>
                </button>
                <button class="icon-button button-danger" type="button" data-action="delete" aria-label="删除${escapeHTML(name)}" data-tooltip="删除账户" ${busy ? "disabled" : ""}>
                  <i data-lucide="trash-2" aria-hidden="true"></i>
                </button>
              </div>
            </td>
          </tr>
        `;
      })
      .join("");
    refreshIcons(elements.accountRows);
  }

  function renderSchedule() {
    const intl = currentMode() === "intl";
    const viewIntl = app.viewVersion === "intl";
    if (elements.versionSwitch) elements.versionSwitch.value = app.viewVersion;
    if (elements.pageTitle) elements.pageTitle.textContent = viewIntl ? "国际版账户" : "国内版账户";
    elements.scheduleEnabled.checked = app.settings.scheduleEnabled;
    elements.scheduleTime.value = app.settings.scheduleTime;
    elements.scheduleTime.disabled = !app.settings.scheduleEnabled;
    elements.scheduleSummary.textContent = app.settings.scheduleEnabled
      ? `每日 ${app.settings.scheduleTime} 执行${intl ? "活跃任务" : "签到任务"}`
      : "当前已关闭";
    elements.versionMode.value = app.settings.mode;
    elements.activeModel.value = app.settings.activeModel;
    elements.recheckDelay.value = String(app.settings.recheckDelayMinutes);
    const proxy = parseProxyURL(app.settings.proxyUrl);
    elements.proxyScheme.value = proxy.scheme;
    elements.proxyHost.value = proxy.host;
    elements.proxyHost.placeholder = proxy.scheme
      ? "host:port（支持认证 user:pass@host:port）"
      : "选择协议后填写网关地址:端口（如 192.168.5.1:1070）";
    elements.activeModelRow.hidden = !intl;
    elements.recheckDelayRow.hidden = !intl;
    // 按钮文案跟随当前查看的版本列表（签到=国内列表 / 活跃=国际列表）
    elements.runAllLabel.textContent = viewIntl ? "全部活跃" : "全部签到";
    if (elements.colDaily) elements.colDaily.textContent = intl ? "今日活跃" : "今日签到";
  }

  function renderAll() {
    renderSummary();
    renderAccounts();
    renderSchedule();
  }

  function setConnection(status, label) {
    elements.connectionStatus.classList.toggle("is-online", status === "online");
    elements.connectionStatus.classList.toggle("is-error", status === "error");
    elements.connectionLabel.textContent = label;
  }

  async function loadState({ quiet = false } = {}) {
    if (!quiet) {
      app.isLoading = true;
      app.loadError = "";
      renderAccounts();
    }
    setConnection("loading", "正在连接");
    elements.refreshState.classList.add("is-spinning");
    elements.refreshState.disabled = true;

    try {
      const payload = await api("/api/state");
      const normalized = normalizeState(payload);
      app.cnAccounts = normalized.cnAccounts;
      app.intlAccounts = normalized.intlAccounts;
      app.accounts = app.viewVersion === "intl" ? app.intlAccounts : app.cnAccounts;
      app.settings = normalized.settings;
      app.loadError = "";
      setConnection("online", "服务正常");
      elements.lastUpdated.textContent = `上次同步 ${timeFormatter.format(new Date())}`;
    } catch (error) {
      app.loadError = getErrorMessage(error, "请确认本地服务正在运行");
      setConnection("error", "连接失败");
      elements.lastUpdated.textContent = "同步失败";
      if (quiet) toast(app.loadError, "error");
    } finally {
      app.isLoading = false;
      elements.refreshState.classList.remove("is-spinning");
      elements.refreshState.disabled = false;
      renderAll();
    }
  }

  function setButtonBusy(button, busy, busyText = "处理中") {
    if (!button) return;
    if (busy) {
      button.dataset.originalHtml = button.innerHTML;
      button.disabled = true;
      button.innerHTML = `<span class="loading-spinner" aria-hidden="true"></span><span>${escapeHTML(busyText)}</span>`;
    } else {
      button.disabled = false;
      if (button.dataset.originalHtml) {
        button.innerHTML = button.dataset.originalHtml;
        delete button.dataset.originalHtml;
      }
      refreshIcons(button);
    }
  }

  async function runAccountAction(account, action) {
    if (app.busyAccounts.has(account.id)) return;
    app.busyAccounts.add(account.id);
    renderAccounts();

    const actionNames = {
      checkin: "签到",
      active: "活跃会话",
      points: "积分查询",
      toggle: account.enabled ? "停用" : "启用",
      delete: "删除",
    };

    try {
      let result;
      if (action === "checkin") {
        result = await api(`/api/accounts/${encodeURIComponent(account.id)}/checkin`, { method: "POST" });
      } else if (action === "active") {
        result = await api(`/api/accounts/${encodeURIComponent(account.id)}/active`, { method: "POST" });
      } else if (action === "points") {
        result = await api(`/api/accounts/${encodeURIComponent(account.id)}/points`, { method: "POST" });
      } else if (action === "toggle") {
        result = await api(`/api/accounts/${encodeURIComponent(account.id)}`, {
          method: "PUT",
          body: JSON.stringify({ enabled: !account.enabled }),
        });
      } else if (action === "delete") {
        result = await api(`/api/accounts/${encodeURIComponent(account.id)}`, { method: "DELETE" });
      }
      toast(result?.message || `${actionNames[action]}成功`);
      await loadState({ quiet: true });
    } catch (error) {
      toast(`${actionNames[action]}失败：${getErrorMessage(error)}`, "error");
      renderAccounts();
    } finally {
      app.busyAccounts.delete(account.id);
      renderAccounts();
    }
  }

  async function runAllCheckins() {
    const intl = app.viewVersion === "intl";
    if (elements.runAll.disabled || !app.accounts.some((account) => account.enabled)) {
      if (!app.accounts.some((account) => account.enabled)) toast("没有可执行的已启用账户", "info");
      return;
    }

    setButtonBusy(elements.runAll, true, intl ? "活跃中" : "签到中");
    try {
      const result = await api("/api/run-all", { method: "POST" });
      const successCount = firstDefined(result?.successCount, result?.succeeded, result?.success);
      const message = typeof successCount === "number"
        ? `批量${intl ? "活跃" : "签到"}完成，成功 ${integerFormatter.format(successCount)} 个账户`
        : result?.message || `批量${intl ? "活跃" : "签到"}已完成`;
      toast(message);
      await loadState({ quiet: true });
    } catch (error) {
      toast(`批量${intl ? "活跃" : "签到"}失败：${getErrorMessage(error)}`, "error");
    } finally {
      setButtonBusy(elements.runAll, false);
    }
  }

  async function refreshAllPoints() {
    if (elements.refreshPointsAll.disabled || !app.accounts.some((account) => account.enabled)) {
      if (!app.accounts.some((account) => account.enabled)) toast("没有已启用的账户", "info");
      return;
    }
    setButtonBusy(elements.refreshPointsAll, true, "刷新中");
    try {
      await api("/api/refresh-points", {
        method: "POST",
        body: JSON.stringify({ version: app.viewVersion }),
      });
      toast("积分已刷新");
      await loadState({ quiet: true });
    } catch (error) {
      toast(`积分刷新失败：${getErrorMessage(error)}`, "error");
    } finally {
      setButtonBusy(elements.refreshPointsAll, false);
    }
  }

  function getAccountById(id) {
    return app.accounts.find((account) => account.id === id);
  }

  function openEditDialog(account) {
    app.editingAccountId = account.id;
    elements.editAccountId.textContent = account.identity || `ID: ${account.id}`;
    elements.nicknameInput.value = String(account.alias || "");
    elements.editDialog.showModal();
    window.setTimeout(() => {
      elements.nicknameInput.focus();
      elements.nicknameInput.select();
    }, 0);
  }

  function closeEditDialog() {
    app.editingAccountId = "";
    if (elements.editDialog.open) elements.editDialog.close();
  }

  async function saveNickname(event) {
    event.preventDefault();
    const account = getAccountById(app.editingAccountId);
    if (!account) return closeEditDialog();

    const nickname = elements.nicknameInput.value.trim();
    setButtonBusy(elements.saveNickname, true, "保存中");
    try {
      const result = await api(`/api/accounts/${encodeURIComponent(account.id)}`, {
        method: "PUT",
        body: JSON.stringify({ enabled: account.enabled, nickname }),
      });
      toast(result?.message || "账户名称已更新");
      closeEditDialog();
      await loadState({ quiet: true });
    } catch (error) {
      toast(`保存失败：${getErrorMessage(error)}`, "error");
    } finally {
      setButtonBusy(elements.saveNickname, false);
    }
  }

  async function saveSchedule(event) {
    event.preventDefault();
    const scheduleEnabled = elements.scheduleEnabled.checked;
    const scheduleTime = normalizeTime(elements.scheduleTime.value);
    const mode = elements.versionMode.value === "intl" ? "intl" : "cn";
    const activeModel = ["hy3", "hy4-preview"].includes(elements.activeModel.value) ? elements.activeModel.value : "hy3";
    const recheckDelayMinutes = clampRecheckDelay(parseNumber(elements.recheckDelay.value) ?? 60);
    const proxyUrl = joinProxyURL(elements.proxyScheme.value, elements.proxyHost.value);
    setButtonBusy(elements.saveSchedule, true, "保存中");

    try {
      const result = await api("/api/settings", {
        method: "PUT",
        body: JSON.stringify({ scheduleEnabled, scheduleTime, mode, activeModel, recheckDelayMinutes, proxyUrl }),
      });
      app.settings = { scheduleEnabled, scheduleTime, mode, activeModel, recheckDelayMinutes, proxyUrl };
      renderSchedule();
      renderAll();
      toast(result?.message || "设置已保存");
    } catch (error) {
      toast(`设置保存失败：${getErrorMessage(error)}`, "error");
      renderSchedule();
    } finally {
      setButtonBusy(elements.saveSchedule, false);
    }
  }

  function setLoginStage(stage, message = "") {
    elements.loginStarting.hidden = stage !== "starting";
    elements.loginReady.hidden = stage !== "ready";
    elements.loginError.hidden = stage !== "error";
    if (stage === "error") elements.loginErrorMessage.textContent = message || "请稍后重试";
    refreshIcons(elements.loginDialog);
  }

  function stopLoginPolling() {
    app.loginPollGeneration += 1;
    if (app.loginPollTimer) {
      window.clearTimeout(app.loginPollTimer);
      app.loginPollTimer = null;
    }
  }

  async function startLogin() {
    stopLoginPolling();
    app.loginId = "";
    app.loginUrl = "";
    setLoginStage("starting");
    if (!elements.loginDialog.open) elements.loginDialog.showModal();

    try {
      const result = await api("/api/login/start", {
        method: "POST",
        body: JSON.stringify({ version: app.viewVersion }),
      });
      app.loginId = String(firstDefined(result?.loginId, result?.id, result?.sessionId, ""));
      app.loginUrl = String(firstDefined(result?.authUrl, result?.url, result?.loginUrl, ""));
      if (!app.loginId || !app.loginUrl) throw new Error("服务未返回有效的登录信息");

      elements.openLoginLink.href = app.loginUrl;
      elements.loginUrlLabel.textContent = compactUrl(app.loginUrl);
      const loginIntl = app.viewVersion === "intl";
      const openLabel = elements.openLoginLink.querySelector("span");
      if (openLabel) openLabel.textContent = loginIntl ? "打开 WorkBuddy 登录页" : "打开腾讯登录页";
      elements.loginPollStatus.className = "poll-status";
      elements.loginPollStatus.innerHTML = '<span class="pulse-dot" aria-hidden="true"></span><span>等待登录完成</span>';
      setLoginStage("ready");
      pollLogin(app.loginPollGeneration);
    } catch (error) {
      setLoginStage("error", getErrorMessage(error));
    }
  }

  function compactUrl(url) {
    try {
      const parsed = new URL(url);
      return `${parsed.host}${parsed.pathname === "/" ? "" : parsed.pathname}`;
    } catch {
      return url.length > 54 ? `${url.slice(0, 51)}...` : url;
    }
  }

  function loginResultState(result) {
    const status = String(firstDefined(result?.status, result?.state, "pending")).toLowerCase();
    if (result?.success === true || result?.authenticated === true || ["success", "complete", "completed", "authenticated", "ok"].includes(status)) {
      return "success";
    }
    if (["error", "failed", "expired", "cancelled", "canceled", "timeout"].includes(status)) return "error";
    return "pending";
  }

  async function pollLogin(generation) {
    if (!app.loginId || generation !== app.loginPollGeneration || !elements.loginDialog.open) return;

    try {
      const result = await api("/api/login/poll", {
        method: "POST",
        body: JSON.stringify({ loginId: app.loginId }),
      });
      if (generation !== app.loginPollGeneration) return;

      const state = loginResultState(result);
      if (state === "success") {
        elements.loginPollStatus.className = "poll-status is-success";
        elements.loginPollStatus.innerHTML = '<span class="pulse-dot" aria-hidden="true"></span><span>登录成功，正在同步账户</span>';
        toast(result?.message || "账户添加成功");
        await loadState({ quiet: true });
        window.setTimeout(() => {
          if (elements.loginDialog.open) elements.loginDialog.close();
        }, 700);
        return;
      }
      if (state === "error") {
        elements.loginPollStatus.className = "poll-status is-error";
        elements.loginPollStatus.innerHTML = `<span class="pulse-dot" aria-hidden="true"></span><span>${escapeHTML(result?.message || "登录已失效，请重试")}</span>`;
        return;
      }

      elements.loginPollStatus.className = "poll-status";
      elements.loginPollStatus.innerHTML = `<span class="pulse-dot" aria-hidden="true"></span><span>${escapeHTML(result?.message || "等待登录完成")}</span>`;
      app.loginPollTimer = window.setTimeout(() => pollLogin(generation), 1800);
    } catch (error) {
      if (generation !== app.loginPollGeneration) return;
      elements.loginPollStatus.className = "poll-status is-error";
      elements.loginPollStatus.innerHTML = `<span class="pulse-dot" aria-hidden="true"></span><span>${escapeHTML(getErrorMessage(error, "状态查询失败，正在重试"))}</span>`;
      app.loginPollTimer = window.setTimeout(() => pollLogin(generation), 3000);
    }
  }

  async function copyLoginLink() {
    if (!app.loginUrl) return;
    try {
      await navigator.clipboard.writeText(app.loginUrl);
      toast("登录链接已复制");
    } catch {
      const input = document.createElement("textarea");
      input.value = app.loginUrl;
      input.style.position = "fixed";
      input.style.opacity = "0";
      document.body.append(input);
      input.select();
      document.execCommand("copy");
      input.remove();
      toast("登录链接已复制");
    }
  }

  function normalizeLogs(payload) {
    if (Array.isArray(payload)) return payload;
    if (Array.isArray(payload?.logs)) return payload.logs;
    if (Array.isArray(payload?.items)) return payload.items;
    if (Array.isArray(payload?.data)) return payload.data;
    if (typeof payload?.raw === "string") return payload.raw.split(/\r?\n/).filter(Boolean);
    return [];
  }

  function normalizeLog(log, index) {
    if (typeof log === "string") {
      const match = log.match(/^\[?([^\]]+?)\]?\s+(DEBUG|INFO|WARN|WARNING|ERROR|SUCCESS)\s+(.*)$/i);
      if (match) return { time: match[1], level: match[2], message: match[3] };
      return { time: "", level: "INFO", message: log, key: index };
    }
    return {
      time: String(firstDefined(log?.time, log?.timestamp, log?.createdAt, "")),
      level: String(firstDefined(log?.level, log?.type, log?.severity, "INFO")),
      message: String(firstDefined(log?.message, log?.msg, log?.text, JSON.stringify(log))),
      key: firstDefined(log?.id, index),
    };
  }

  function formatLogTime(value) {
    if (!value) return "--:--:--";
    const date = new Date(value);
    if (!Number.isNaN(date.getTime())) return timeFormatter.format(date);
    const timeMatch = String(value).match(/\b(\d{2}:\d{2}(?::\d{2})?)\b/);
    return timeMatch ? timeMatch[1] : String(value).slice(0, 8);
  }

  function renderLogs(logs) {
    if (!logs.length) {
      elements.logList.innerHTML = `
        <div class="drawer-empty">
          <i data-lucide="file-clock" aria-hidden="true"></i>
          <strong>暂无运行记录</strong>
          <span>任务执行后会显示在这里</span>
        </div>`;
      elements.logSubtitle.textContent = "暂无记录";
      refreshIcons(elements.logList);
      return;
    }

    const normalized = logs.map(normalizeLog);
    elements.logSubtitle.textContent = `最近 ${integerFormatter.format(normalized.length)} 条`;
    elements.logList.innerHTML = normalized
      .map((log) => {
        const level = log.level.toLowerCase();
        const levelClass = level.includes("error") || level.includes("fail")
          ? "is-error"
          : level.includes("warn")
            ? "is-warn"
            : level.includes("success") || level.includes("ok")
              ? "is-success"
              : "";
        return `
          <div class="log-entry">
            <time class="log-time">${escapeHTML(formatLogTime(log.time))}</time>
            <span class="log-level ${levelClass}">${escapeHTML(log.level)}</span>
            <span class="log-message">${escapeHTML(log.message)}</span>
          </div>`;
      })
      .join("");
  }

  async function loadLogs() {
    elements.refreshLogs.classList.add("is-spinning");
    elements.refreshLogs.disabled = true;
    elements.logList.innerHTML = '<div class="drawer-loading"><div class="loading-spinner" aria-hidden="true"></div><span>正在读取日志</span></div>';
    try {
      const result = await api("/api/logs");
      renderLogs(normalizeLogs(result));
    } catch (error) {
      elements.logList.innerHTML = `
        <div class="drawer-empty">
          <i data-lucide="circle-alert" aria-hidden="true"></i>
          <strong>日志读取失败</strong>
          <span>${escapeHTML(getErrorMessage(error))}</span>
        </div>`;
      elements.logSubtitle.textContent = "读取失败";
      refreshIcons(elements.logList);
    } finally {
      elements.refreshLogs.classList.remove("is-spinning");
      elements.refreshLogs.disabled = false;
    }
  }

  function openLogDrawer() {
    elements.drawerBackdrop.hidden = false;
    elements.drawer.setAttribute("aria-hidden", "false");
    requestAnimationFrame(() => {
      elements.drawerBackdrop.classList.add("is-visible");
      elements.drawer.classList.add("is-open");
    });
    document.body.style.overflow = "hidden";
    loadLogs();
  }

  function closeLogDrawer() {
    elements.drawerBackdrop.classList.remove("is-visible");
    elements.drawer.classList.remove("is-open");
    elements.drawer.setAttribute("aria-hidden", "true");
    document.body.style.overflow = "";
    window.setTimeout(() => {
      if (!elements.drawer.classList.contains("is-open")) elements.drawerBackdrop.hidden = true;
    }, 200);
  }

  function toast(message, type = "success") {
    const node = document.createElement("div");
    const icon = type === "error" ? "circle-alert" : type === "info" ? "info" : "circle-check";
    node.className = `toast is-${type}`;
    node.innerHTML = `
      <i data-lucide="${icon}" aria-hidden="true"></i>
      <span class="toast-message">${escapeHTML(message)}</span>
      <button class="toast-close" type="button" aria-label="关闭提示"><i data-lucide="x" aria-hidden="true"></i></button>
    `;
    elements.toastRegion.append(node);
    refreshIcons(node);

    let timer = window.setTimeout(() => dismissToast(node), type === "error" ? 6000 : 4000);
    node.querySelector(".toast-close").addEventListener("click", () => {
      window.clearTimeout(timer);
      dismissToast(node);
    });
  }

  function dismissToast(node) {
    if (!node?.isConnected) return;
    node.classList.add("is-leaving");
    window.setTimeout(() => node.remove(), 170);
  }

  function bindEvents() {
    elements.refreshState.addEventListener("click", () => loadState({ quiet: true }));
    elements.runAll.addEventListener("click", runAllCheckins);
    elements.refreshPointsAll.addEventListener("click", refreshAllPoints);
    // 两个版本统一走浏览器授权登录（国际版为 workbuddy.ai 授权页）
    elements.addAccount.addEventListener("click", startLogin);
    elements.search.addEventListener("input", renderAccounts);
    elements.statusFilter.addEventListener("change", renderAccounts);
    elements.scheduleForm.addEventListener("submit", saveSchedule);
    // 页头版本切换：直接切换账号列表（国内版/国际版独立容器）
    elements.versionSwitch.addEventListener("change", () => {
      app.viewVersion = elements.versionSwitch.value === "intl" ? "intl" : "cn";
      app.accounts = app.viewVersion === "intl" ? app.intlAccounts : app.cnAccounts;
      elements.lastUpdated.textContent = `上次同步 ${timeFormatter.format(new Date())}`;
      renderAll();
    });
    elements.scheduleEnabled.addEventListener("change", () => {
      elements.scheduleTime.disabled = !elements.scheduleEnabled.checked;
    });

    elements.accountRows.addEventListener("click", (event) => {
      const button = event.target.closest("[data-action]");
      if (!button || button.matches('input[type="checkbox"]')) return;
      const row = button.closest("[data-account-id]");
      const account = row ? getAccountById(row.dataset.accountId) : null;
      if (!account) return;

      const action = button.dataset.action;
      if (action === "edit") {
        openEditDialog(account);
      } else if (action === "delete") {
        const confirmed = window.confirm(`确定删除“${accountDisplayName(account)}”吗？此操作无法撤销。`);
        if (confirmed) runAccountAction(account, "delete");
      } else {
        runAccountAction(account, action);
      }
    });

    elements.accountRows.addEventListener("change", (event) => {
      const toggle = event.target.closest('input[data-action="toggle"]');
      if (!toggle) return;
      const row = toggle.closest("[data-account-id]");
      const account = row ? getAccountById(row.dataset.accountId) : null;
      if (!account) return;
      toggle.checked = account.enabled;
      runAccountAction(account, "toggle");
    });

    elements.tableState.addEventListener("click", (event) => {
      const button = event.target.closest("[data-state-action]");
      if (!button) return;
      if (button.dataset.stateAction === "error") loadState();
      if (button.dataset.stateAction === "empty") startLogin();
    });

    elements.openLogs.addEventListener("click", openLogDrawer);
    elements.closeLogs.addEventListener("click", closeLogDrawer);
    elements.drawerBackdrop.addEventListener("click", closeLogDrawer);
    elements.refreshLogs.addEventListener("click", loadLogs);

    elements.loginForm.addEventListener("submit", (event) => {
      if (event.submitter?.value === "cancel") stopLoginPolling();
    });
    elements.loginDialog.addEventListener("close", stopLoginPolling);
    elements.loginDialog.addEventListener("cancel", stopLoginPolling);
    elements.retryLogin.addEventListener("click", startLogin);
    elements.copyLoginLink.addEventListener("click", copyLoginLink);

    elements.editForm.addEventListener("submit", saveNickname);
    elements.closeEdit.addEventListener("click", closeEditDialog);
    elements.cancelEdit.addEventListener("click", closeEditDialog);
    elements.editDialog.addEventListener("cancel", () => {
      app.editingAccountId = "";
    });
    elements.editDialog.addEventListener("close", () => {
      app.editingAccountId = "";
    });

    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && elements.drawer.classList.contains("is-open")) closeLogDrawer();
    });
  }

  bindEvents();
  refreshIcons();
  loadState();
})();
