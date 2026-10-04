async function setLoginMode(mode, preserveError) {
  const ticket = ++loginModeRequest;
  loginMode = mode === "account" ? "account" : "admin";
  const route = pageRoute();
  const tab = route.role === loginMode ? route.tab : loginMode === "account" ? "subscription" : "keys";
  writePageRoute(loginMode, tab);
  const account = loginMode === "account";
  setText(document.querySelector("title"), m(account ? "ui.api_key_usage" : "ui.api_key_billing"));
  $("gate-mode-admin").classList.toggle("active", !account);
  $("gate-mode-admin").setAttribute("aria-selected", String(!account));
  $("gate-mode-account").classList.toggle("active", account);
  $("gate-mode-account").setAttribute("aria-selected", String(account));
  setText($("gate-key-label"), account ? "API Key" : m("ui.management_key"));
  $("gate-username").value = account ? "api-key" : "administrator";
  setTextAttribute($("gate-key"), "placeholder", account ? m("ui.enter_api_key") : m("ui.enter_management_key"));
  setText($("gate-submit"), account ? m("ui.view_my_usage") : m("ui.open_management_console"));
  $("gate-key").value = "";
  if (!preserveError) {
    setText($("gate-error"), "");
    $("gate-error").className = "notice err";
  }
  const saved = await recalledCredential(loginMode);
  if (ticket !== loginModeRequest) return;
  if (saved) setTextAttribute($("gate-key"), "placeholder", m("ui.key_remembered"));
  $("gate-remember").checked = !!saved;
}

function showGate(mode, error) {
  sessionGeneration++;
  hideNotice();
  hideCostTooltip();
  try { sessionStorage.removeItem(SESSION_CREDENTIAL_PREFIX + mode); } catch (_) {}
  for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close();
  for (const controller of activeRequests) controller.abort();
  if (currentRole) {
    clearCredentialScope(currentRole === "account");
    resetRoleData(currentRole);
  }
  authKey = "";
  currentRole = "";
  document.body.classList.remove("privacy-masked");
  $("app").classList.add("hidden");
  $("account-app").classList.add("hidden");
  $("gate").classList.remove("hidden");
  const box = $("gate-error");
  setText(box, error || "");
  box.className = error ? "notice err show" : "notice err";
  setLoginMode(mode, true);
}

async function submitLogin() {
  const role = loginMode;
  const generation = sessionGeneration;
  let key = $("gate-key").value.trim();
  if (!key) key = await recalledCredential(role);
  if (!key) {
    setText($("gate-error"), role === "account" ? m("ui.enter_an_api_key") : m("ui.enter_a_management_key"));
    $("gate-error").className = "notice err show";
    return;
  }
  $("gate-submit").disabled = true;
  try {
    if (role === "account") await startAccount(key); else await startAdmin(key);
    if (generation !== sessionGeneration) return;
    try { await saveCredential(role, key, $("gate-remember").checked); } catch (err) {
      if (generation !== sessionGeneration) return;
      const message = m("ui.signed_in_but_the_session_was_not_saved_value", { v0: err.message || err });
      notify(message, "err");
    }
    $("gate-key").value = "";
  } catch (err) {
    if (generation !== sessionGeneration || err instanceof StaleRequestError) return;
    if (err instanceof AuthError) await fail(err); else showGate(role, m("ui.sign_in_failed_value", { v0: err.message || err }));
  } finally { $("gate-submit").disabled = false; }
}

$("gate-form").onsubmit = (event) => {
  event.preventDefault();
  submitLogin();
};
$("gate-mode-admin").onclick = () => setLoginMode("admin");
$("gate-mode-account").onclick = () => setLoginMode("account");

async function startAdmin(key) {
  setText(document.querySelector("title"), m("ui.api_key_billing"));
  const generation = sessionGeneration;
  currentRole = "admin";
  authKey = key;
  await activateCredentialScope(false, key);
  if (generation !== sessionGeneration) throw new StaleRequestError();
  restoreRolePreferences("admin");
  const route = pageRoute();
  setAdminTab(route.role === "admin" ? route.tab : "keys");
  $("gate").classList.add("hidden");
  $("account-app").classList.add("hidden");
  $("app").classList.remove("hidden");
  schedulePageTabLayout();
  await loadPage(false).catch(fail);
  // Enroll the current management session so the external worker can resume after CPA restarts.
  guard(() => plugin("POST", "/credential-task-session?origin=" + encodeURIComponent(location.origin)));
}

async function startAccount(key) {
  setText(document.querySelector("title"), m("ui.api_key_usage"));
  const generation = sessionGeneration;
  currentRole = "account";
  authKey = key;
  // CLIProxyAPI remains authoritative for whether this downstream key is
  // currently valid. The plugin endpoint only uses the key to select its hash.
  resources.account.models.value = await fetchModels(true, key);
  await activateCredentialScope(true, key);
  if (generation !== sessionGeneration) throw new StaleRequestError();
  restoreRolePreferences("account");
  $("gate").classList.add("hidden");
  $("app").classList.add("hidden");
  $("account-app").classList.remove("hidden");
  setText($("account-identity"), previewCredential(key));
  const route = pageRoute();
  setAccountTab(route.role === "account" ? route.tab : "subscription");
  await loadPage(true).catch(fail);
  schedulePageTabLayout();
}

function restoreRolePreferences(role) {
  const account = role === "account";
  const prefix = account ? "account-" : "";
  // Dynamic choices must exist before the first filtered request, even
  // when the current result no longer contains the stored selection.
  for (const id of ["request-event-model", "request-event-source", "error-model", "error-source", "error-type"])
    restoreChoice(role, prefix + id, "", true);
  restoreChoice(role, prefix + "request-event-status", "");
  restoreChoice(role, prefix + "auth-enabled-only", true);
  restoreChoice(role, prefix + "auth-provider", "");
  setPrivacyMask(role, viewPreference(role, prefix + "privacy-mask", false));
  if (!account) {
    restoreChoice(role, "analysis-key", "", true);
    if ($("analysis-key").value === UNASSIGNED_KEY) $("analysis-key").selectedOptions[0].textContent = m("ui.unassigned");
    restoreChoice(role, "keys-include-deleted", false);
    restoreChoice(role, "plugin-log-level", "all");
  }
  sharedTimeRanges[role] = storedTimeRange(role);
  const range = sharedTimeRanges[role];
  const rolling = rollingTimeMode(range.mode);
  sharedTimeChoices[role] = { hour: 8, day: 7, custom: range.mode === "custom" ? range : null };
  if (rolling) sharedTimeChoices[role][rolling.unit] = rolling.value;
  const defaultDimension = account ? "models" : "api_keys";
  const dimension = viewPreference(role, "analysis-dimension", defaultDimension);
  const allowedDimensions = account ? ["models", "sources"] : ["api_keys", "models", "sources"];
  analysisDimensions[role] = allowedDimensions.includes(dimension) ? dimension : defaultDimension;
  const metric = viewPreference(role, "analysis-metric", "total_tokens");
  analysisMetrics[role] = Object.hasOwn(DISTRIBUTION_METRICS, metric) ? metric : "total_tokens";
  updateSharedTimeControl(account);
  updateFilterClearButton($(prefix + "request-events-filter-clear"));
  updateFilterClearButton($(prefix + "errors-filter-clear"));
}

function resetRoleControls(role) {
  const account = role === "account";
  const prefix = account ? "account-" : "";
  closeSharedTimePopover(account);
  setPrivacyMask(role, false);
  $(prefix + "auth-search").value = "";
  if (!account) {
    $("key-filter").value = "";
    $("keys-include-deleted").checked = false;
    $("price-filter").value = "";
    $("plugin-log-filter").value = "";
  }
}

function resetRoleData(role) {
  const account = role === "account";
  destroyAnalysisCharts(account);
  if (role === "account") accountUIState = emptyAuthQuotaState();
  else {
    $("route-picker").close();
    adminUIState = emptyAdminUIState();
    expandedPluginLogIDs.clear();
    resourceGroups.admin.configuration.syncErrors = [];
    keyDirectory.clear();
  }
  for (const entry of Object.values(resources[role])) Object.assign(entry, { value: null, error: "" });
  for (const group of Object.values(resourceGroups[role])) group.task = null;
  for (const page of Object.values(pages[role])) {
    page.task = null;
    for (const section of page.sections) section.displayed = null;
  }
  analysisQueries[role].clear();
  if (!account) eventKeysQuery.clear();
  for (const list of Object.values(pagedLists[role])) list.reset();
  resetRoleControls(role);
}

function privacyMasked() { return document.body.classList.contains("privacy-masked"); }

function privacyMaskButton(role) { return $((role === "account" ? "account-" : "") + "privacy-mask"); }

function setPrivacyMask(role, masked) {
  const button = privacyMaskButton(role);
  button.setAttribute("aria-pressed", String(masked));
  // setActionIcon would replace the descriptive title.
  button.replaceChildren(actionIcon(masked ? "eye-off" : "eye"));
  setTextAttribute(button, "aria-label", m("ui.mask"));
  document.body.classList.toggle("privacy-masked", masked);
}

function activeTab(account) {
  return account ? accountTab : document.querySelector(".tabs button[data-tab].active")?.dataset.tab || "keys";
}

const PAGE_TAB_ROWS = Array.from(document.querySelectorAll(".page-tabs-row"));
let tabLayoutFrame = 0;

// The select stands in for the tab bar when the tabs do not fit; it mirrors their labels and active tab.
for (const row of PAGE_TAB_ROWS) {
  const select = row.querySelector(".page-tab-select > select");
  const tabs = Array.from(row.querySelectorAll(".tabs > button"));
  const tabName = (button) => button.dataset.tab || button.dataset.accountTab;
  select.replaceChildren(...tabs.map((button) => el("option", { value: tabName(button) }, window.billingI18n.boundText(button))));
  select.onchange = () => {
    tabs.find((button) => tabName(button) === select.value).click();
    // A refused switch leaves the previous tab active.
    select.value = tabName(tabs.find((button) => button.classList.contains("active")));
  };
}

function updatePageTabLayouts() {
  tabLayoutFrame = 0;
  for (const row of PAGE_TAB_ROWS) {
    if (!row.getClientRects().length) continue;
    const switcher = row.querySelector(".page-tab-switcher");
    const tabBar = switcher.querySelector(".tabs");
    const select = switcher.querySelector("select");
    const compact = tabBar.scrollWidth > switcher.clientWidth + 1;
    const focusedTab = tabBar.contains(document.activeElement);
    const focusedSelect = document.activeElement === select;
    row.querySelector(".page-navigation").classList.toggle("compact", compact);
    // Keyboard focus moves to whichever control is now shown.
    if (compact && focusedTab) select.focus({ preventScroll: true });
    if (!compact && focusedSelect) tabBar.querySelector(".active").focus({ preventScroll: true });
    const actions = row.querySelector(".page-tab-actions");
    const hasControls = Array.from(actions.children).some((node) => !node.classList.contains("hidden"));
    actions.classList.toggle("hidden", !hasControls);
    const card = Array.from(row.parentElement.querySelectorAll(".event-list-card")).find((card) => card.getClientRects().length);
    if (card) row.parentElement.style.setProperty("--event-list-top", card.getBoundingClientRect().top + scrollY + "px");
    positionSharedTimePopover(!!row.closest("#account-app"));
  }
}

function schedulePageTabLayout() {
  if (tabLayoutFrame) return;
  tabLayoutFrame = requestAnimationFrame(updatePageTabLayouts);
}

addEventListener("resize", schedulePageTabLayout);
const pageTabResizeObserver = new ResizeObserver(schedulePageTabLayout);
for (const element of document.querySelectorAll(".head, #admin-data-status, #account-data-status, .page-tabs-row, .page-tabs-row .tabs")) {
  pageTabResizeObserver.observe(element, { box: "border-box" });
}

function setAccountTab(tab) {
  accountTab = ACCOUNT_TAB_IDS.includes(tab) ? tab : "subscription";
  writePageRoute("account", accountTab);
  document.querySelectorAll("[data-account-tab]").forEach((button) => {
    const active = button.dataset.accountTab === accountTab;
    button.classList.toggle("active", active);
    button.setAttribute("aria-current", active ? "page" : "false");
  });
  $("account-page-tab-select").value = accountTab;
  for (const name of ACCOUNT_TAB_IDS) { $("account-tab-" + name).classList.toggle("hidden", name !== accountTab); }
  $("account-shared-time-filter").classList.toggle("hidden", !["analysis", "request-events", "errors"].includes(accountTab));
  $("account-events-export").classList.toggle("hidden", !["request-events", "errors"].includes(accountTab));
  if (!["analysis", "request-events", "errors"].includes(accountTab)) { closeSharedTimePopover(true); }
  schedulePageTabLayout();
}

// Keep the shared policy separate from per-file host weights; saves use the existing management session.
function isGPTModel(id) { return /^gpt-/i.test(String(id || "").split("/").pop()); }
function syncCredentialSchedule() {
  const daily = $("credential-task-mode").value === "daily";
  $("credential-time-field").classList.toggle("hidden", !daily);
  $("credential-interval-field").classList.toggle("hidden", daily);
}
function credentialConcurrencyText(file) {
  return m("ui.credential_concurrency_status", {
    v0: int(file.current_concurrency),
    v1: file.max_concurrency ? int(file.max_concurrency) : m("ui.unlimited")
  });
}
// These timestamps come from the worker; refreshing never advances the schedule.
function renderCredentialTimes(result) {
  const format = (value) =>
    new Date(value).toLocaleString(locale(), { timeZone: result.settings.timezone, hour12: false, timeZoneName: "short" });
  const last = $("credential-last-run"), next = $("credential-next-run");
  last.textContent = result.last_run_at ? format(result.last_run_at) : m("ui.credential_never_run");
  last.dateTime = result.last_run_at || "";
  next.textContent = result.next_run_at
    ? format(result.next_run_at)
    : m(
        !result.settings.enabled
          ? "ui.disabled"
          : result.executing ? "ui.credential_executing" : result.running ? "ui.credential_scheduling" : "ui.credential_task_waiting"
      );
  next.dateTime = result.next_run_at || "";
}
// Refresh live status only, leaving unsaved form values and weight inputs untouched.
let credentialStatusLoading = false;
setInterval(async () => {
  if (credentialStatusLoading || document.hidden || currentRole !== "admin") return;
  const tab = activeTab(false), generation = sessionGeneration;
  if (!["settings", "auth-files"].includes(tab)) return;
  credentialStatusLoading = true;
  try {
    const result = await plugin("GET", tab === "settings" ? "/credential-settings" : "/auth-files");
    if (generation !== sessionGeneration || currentRole !== "admin" || activeTab(false) !== tab) return;
    if (tab === "settings") {
      $("credential-session-ttl-field").classList.toggle("hidden", !result.sticky_sessions);
      renderCredentialTimes(result);
    } else {
      const files = new Map((result.files || []).map((file) => [file.auth_index, file]));
      for (const node of $("auth-files-body").querySelectorAll(".auth-file-concurrency")) {
        const file = files.get(node.dataset.authIndex);
        if (file) setText(node, credentialConcurrencyText(file));
      }
    }
  } catch (_) {
    /* A temporary status failure must not discard an edit. */
  } finally { credentialStatusLoading = false; }
}, 5000);
async function loadCredentialSettings() {
  $("credential-settings-fields").disabled = true;
  const result = await plugin("GET", "/credential-settings");
  const cfg = result.settings;
  // The host switch governs both binding reservations and TTL visibility.
  $("credential-session-ttl-field").classList.toggle("hidden", !result.sticky_sessions);
  renderCredentialTimes(result);
  $("credential-max-concurrency").value = cfg.max_concurrency;
  $("credential-max-concurrency").setCustomValidity("");
  $("credential-session-ttl").value = cfg.session_ttl_minutes ?? 5;
  $("credential-task-enabled").checked = cfg.enabled;
  $("credential-task-mode").value = cfg.mode;
  $("credential-task-interval").value = cfg.interval_minutes;
  $("credential-task-time").value = cfg.time_of_day;
  $("credential-task-timezone").value = cfg.timezone;
  $("credential-task-model").value = cfg.model;
  $("credential-task-prompt").value = cfg.prompt;
  $("credential-task-models").replaceChildren(
    ...(resources.admin.models.value || []).filter(isGPTModel).map((model) => el("option", { value: model }))
  );
  setText(
    $("credential-settings-status"),
    result.error || (cfg.enabled ? m(result.running ? "ui.credential_task_running" : "ui.credential_task_waiting") : "")
  );
  syncCredentialSchedule();
  $("credential-settings-fields").disabled = false;
}
// Small positive limits now represent ordinary capacity without subtracting reserved slots.
$("credential-max-concurrency").oninput = () => {
  const input = $("credential-max-concurrency");
  input.setCustomValidity(Number(input.value) < 0 ? m("ui.credential_concurrency_invalid") : "");
};
$("credential-task-mode").onchange = syncCredentialSchedule;
$("credential-settings-form").onsubmit = (event) => {
  event.preventDefault();
  guard(async () => {
    const cfg = {
      max_concurrency: Number($("credential-max-concurrency").value),
      session_ttl_minutes: Number($("credential-session-ttl").value),
      enabled: $("credential-task-enabled").checked,
      mode: $("credential-task-mode").value,
      interval_minutes: Number($("credential-task-interval").value),
      time_of_day: $("credential-task-time").value,
      timezone: $("credential-task-timezone").value.trim(),
      model: $("credential-task-model").value.trim(),
      prompt: $("credential-task-prompt").value.trim()
    };
    $("credential-settings-save").disabled = true;
    try {
      const result = await plugin("PUT", "/credential-settings?origin=" + encodeURIComponent(location.origin), cfg);
      renderCredentialTimes(result);
      setText($("credential-settings-status"), result.error || m("ui.saved"));
    } finally { $("credential-settings-save").disabled = false; }
  });
};
function credentialWeightInput(file) {
  let timer, saving = false, pending = null;
  const input = el("input", {
    type: "number",
    min: 0,
    max: 1000000,
    value: file.weight ?? 1,
    "aria-label": m("ui.credential_call_weight"),
    title: m("ui.credential_call_weight")
  });
  const status = el("span", { class: "credential-weight-status small muted", role: "status" });
  const save = async () => {
    const weight = pending;
    if (saving || weight === null) return;
    pending = null;
    saving = true;
    setText(status, m("ui.saving"));
    try {
      await api("PATCH", "/v0/management/auth-files/fields", { name: file.name, weight });
      file.weight = weight;
      setText(status, m("ui.saved"));
    } catch (error) { setText(status, error.message); } finally {
      saving = false;
      if (pending !== null) await save();
    }
  };
  input.oninput = () => {
    clearTimeout(timer);
    if (!input.checkValidity() || input.value === "") {
      pending = null;
      return;
    }
    pending = Number(input.value);
    timer = setTimeout(save, 500);
  };
  input.onchange = () => {
    clearTimeout(timer);
    if (pending !== null) guard(save);
  };
  return el(
    "label",
    { class: "credential-weight" },
    el("span", {
      class: "credential-weight-help",
      text: m("ui.credential_call_weight"),
      ...hoverTooltipProps(() => [el("div", { class: "hover-tooltip-body", text: m("ui.credential_weight_help") })], true)
    }),
    input,
    status
  );
}

// Native form validation and server validation both enforce the retention floor.
$("database-cleanup-form").onsubmit = (event) => {
  event.preventDefault();
  const days = Number($("database-retention-days").value);
  openActionDialog(m("ui.database_cleanup"), m("ui.database_cleanup_confirm", { v0: days }), undefined, async () => {
    const result = await plugin("DELETE", "/database/request-events", { days });
    setText($("database-cleanup-status"), m("ui.database_cleanup_done", { v0: result.cleared }));
    await pagedList("events").load(true);
    await pagedList("errors").load(true);
  });
};

const TABS = Array.from(document.querySelectorAll(".tabs button[data-tab]"));
function setAdminTab(name) {
  if (editors.get($("route-picker"))?.saving) return;
  const selected = ADMIN_TAB_IDS.includes(name) ? name : "keys";
  if (selected !== "keys") $("route-picker").close();
  writePageRoute("admin", selected);
  for (const tab of TABS) {
    const active = tab.dataset.tab === selected;
    tab.classList.toggle("active", active);
    tab.setAttribute("aria-current", active ? "page" : "false");
  }
  $("page-tab-select").value = selected;
  const visible =
    selected === "settings"
      ? new Set(["credential-settings", "plans", "routes", "prices", "database", "plugin-logs"])
      : new Set([selected]);
  for (const section of [
    "credential-settings",
    "keys",
    "analysis",
    "plans",
    "routes",
    "prices",
    "request-events",
    "errors",
    "auth-files",
    "database",
    "plugin-logs"
  ]) { $("tab-" + section).classList.toggle("hidden", !visible.has(section)); }
  $("shared-time-filter").classList.toggle("hidden", !["analysis", "request-events", "errors"].includes(selected));
  $("events-export").classList.toggle("hidden", !["request-events", "errors"].includes(selected));
  if (!["analysis", "request-events", "errors"].includes(selected)) { closeSharedTimePopover(false); }
  schedulePageTabLayout();
}
for (const tab of TABS)
  tab.onclick = () => {
    if (activeTab(false) === tab.dataset.tab) return;
    setAdminTab(tab.dataset.tab);
    guard(() => loadPage(false));
  };

document.querySelectorAll("[data-account-tab]").forEach((button) => {
  button.onclick = () => {
    if (activeTab(true) === button.dataset.accountTab) return;
    setAccountTab(button.dataset.accountTab);
    guard(() => loadPage(true));
  };
});
$("analysis-key").addEventListener("change", () =>
  guard(() => {
    saveChoice("admin", "analysis-key");
    return loadPage(false);
  })
);

const THEME_MODES = {
  "": { icon: "sun-moon", label: () => m("ui.theme_follows_system") },
  white: { icon: "sun", label: () => m("ui.theme_light") },
  dark: { icon: "moon", label: () => m("ui.theme_dark") }
};
function updateThemeButtons() {
  const mode = THEME_MODES[window.billingTheme?.preference()] || THEME_MODES[""];
  document.querySelectorAll('[data-page-action="theme"]').forEach((button) => setActionIcon(button, mode.icon, mode.label()));
}

function updateThemedVisuals() {
  updateThemeButtons();
  if (currentRole && activeTab(currentRole === "account") === "analysis") renderAnalysis(currentRole === "account");
}

for (const button of document.querySelectorAll('[data-page-action="refresh"]')) {
  setActionIcon(button, "refresh", m("ui.refresh"));
  button.onclick = () => refreshPage();
}
document.querySelectorAll('[data-page-action="theme"]').forEach((button) => {
  button.onclick = () => {
    window.billingTheme?.cycle();
    updateThemeButtons();
  };
});
document.querySelectorAll('[data-page-action="logout"]').forEach((button) => {
  setActionIcon(button, "logout", m("ui.sign_out"));
  button.onclick = () =>
    guard(async () => {
      const role = currentRole || loginMode;
      await deleteCredential(role);
      const preference = viewPreferenceKey(role, (role === "account" ? "account-" : "") + "privacy-mask");
      if (preference) { try { localStorage.removeItem(preference); } catch (_) {} }
      showGate(role, "");
    });
});
function addLanguageControl(group) {
  const menu = el("div", { class: "language-menu", popover: "auto", role: "menu", "aria-label": m("language.select") });
  const button = el(
    "button",
    {
      type: "button",
      class: "icon-action",
      "data-page-action": "language",
      "aria-haspopup": "menu",
      "aria-expanded": "false",
      "aria-label": m("language.select"),
      title: m("language.select"),
      onclick: () => {
        if (menu.matches(":popover-open")) return menu.hidePopover();
        const current = window.billingI18n.current();
        menu.replaceChildren(
          ...window.billingI18n.languages().map((item) =>
            el("button", {
              type: "button",
              class: "language-option",
              role: "menuitemradio",
              "aria-checked": String(item.value === current),
              text: item.label,
              onclick: () => {
                menu.hidePopover();
                button.focus();
                window.billingI18n.select(item.value);
              }
            })
          )
        );
        menu.showPopover();
        positionAnchoredPopover(menu, button, true);
        menu.querySelector('[aria-checked="true"]')?.focus({ preventScroll: true });
      }
    },
    actionIcon("globe")
  );
  menu.addEventListener("toggle", (event) => button.setAttribute("aria-expanded", String(event.newState === "open")));
  menu.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      menu.hidePopover();
      button.focus();
      return;
    }
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    event.preventDefault();
    const items = Array.from(menu.querySelectorAll("button"));
    const index = items.indexOf(document.activeElement);
    items[(index + (event.key === "ArrowDown" ? 1 : items.length - 1)) % items.length].focus();
  });
  group.prepend(button, menu);
}
document.querySelectorAll(".standalone-actions").forEach(addLanguageControl);

window.addEventListener("billing-theme-change", updateThemedVisuals);
window.addEventListener("billing-language-change", () => {
  dateFormatter = new Intl.DateTimeFormat(locale());
  timeFormatter = new Intl.DateTimeFormat(locale(), { hour: "numeric", minute: "numeric", second: "numeric", hour12: false });
  updateThemeButtons();
  updateSharedTimeControl(false);
  updateSharedTimeControl(true);
  renderPage();
  schedulePageTabLayout();
});

function updateRefreshButtons() {
  const page = currentPage();
  const label = currentRole
    ? document.querySelector(currentRole === "account" ? "[data-account-tab].active" : "[data-tab].active")?.textContent.trim()
    : "";
  for (const button of document.querySelectorAll('[data-page-action="refresh"]')) {
    button.disabled = !!page?.task;
    setTextAttribute(
      button,
      "title",
      setTextAttribute(button, "aria-label", label ? m("ui.refresh_value", { v0: label }) : m("ui.refresh"))
    );
  }
}

function refreshPage() {
  if (!currentRole) return Promise.resolve();
  return currentPage().task || guard(() => loadPage(currentRole === "account", { reload: true }));
}

for (const role of ["admin", "account"]) {
  const button = privacyMaskButton(role);
  setPrivacyMask(role, false);
  button.onclick = () => {
    setPrivacyMask(role, !privacyMasked());
    saveViewPreference(role, button.id, privacyMasked());
  };
}

(async function () {
  const panelKey = readPanelKey();
  document.querySelectorAll('input[type="checkbox"]').forEach((input) => {
    const placeholder = document.createTextNode("");
    input.replaceWith(placeholder);
    placeholder.replaceWith(input.getAttribute("role") === "switch" ? switchControl({ input }) : checkboxControl({ input }));
  });
  document.querySelectorAll("[data-lucide-icon]")
    .forEach((placeholder) => placeholder.replaceWith(actionIcon(placeholder.dataset.lucideIcon, placeholder.className)));
  updateThemeButtons();
  try {
    if (embedded && panelKey && initialRoute.role !== "account") {
      await startAdmin(panelKey);
      return;
    }
    const saved = await recalledCredential(loginMode, true);
    if (!saved) {
      showGate(loginMode, "");
      return;
    }
    if (loginMode === "account") await startAccount(saved); else await startAdmin(saved);
  } catch (err) {
    if (err instanceof AuthError) await fail(err); else showGate(loginMode, m("ui.sign_in_failed_value", { v0: err.message || err }));
  }
})();
