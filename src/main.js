import { applyTranslations, getLocale, setLocale, t } from "/i18n.js";

const tauri = window.__TAURI__;
const invoke = tauri?.core?.invoke;

const els = {};

let baseUrl = "http://127.0.0.1:8787";
let config = { channels: [], model_mappings: [], health_check: {}, listen_addr: "127.0.0.1:8787" };
let channelsState = [];
let codexClientVersion = "";
let tokens = [];
let modalType = null;
let modalId = null;
let lanAddrs = [];
let routerSwitchBusy = false;
let usageEventSource = null;
let usageRefreshTimer = null;
let usageLoading = false;
let lastProcessStatus = null;
let lastUsageData = null;
let codexStatus = null;
let codexBackups = [];
let codexBusy = false;
let terminalEventSource = null;

function statusLabel(status) {
  const key = ["healthy", "unknown", "unhealthy", "disabled"].includes(status) ? status : "unknown";
  return t(`status.${key}`);
}

function esc(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function toast(message) {
  els.toast.textContent = message;
  els.toast.classList.add("show");
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => els.toast.classList.remove("show"), 2400);
}

async function copyText(text) {
  if (!text) {
    toast(t("common.copyEmpty"));
    return;
  }
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const area = document.createElement("textarea");
      area.value = text;
      area.style.position = "fixed";
      area.style.opacity = "0";
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      area.remove();
    }
    toast(t("common.copied"));
  } catch {
    toast(t("common.copyFailed"));
  }
}

async function safeInvoke(cmd, args) {
  if (!invoke) return null;
  try {
    return await invoke(cmd, args);
  } catch {
    return null;
  }
}

async function openExternal(url) {
  if (!url) return;
  try {
    if (tauri?.shell?.open) {
      await tauri.shell.open(url);
      return;
    }
    window.open(url, "_blank", "noopener,noreferrer");
  } catch {
    await copyText(url);
  }
}

function renderProcess(status) {
  lastProcessStatus = status;
  if (!status) {
    els.routerToggle.checked = false;
    els.routerToggle.disabled = routerSwitchBusy;
    els.processState.textContent = t("router.checking");
    return;
  }
  const running = Boolean(status?.running);
  els.routerToggle.checked = running;
  els.routerToggle.disabled = routerSwitchBusy;
  els.processState.textContent = running
    ? t("router.running", { pid: status.pid ? ` (PID ${status.pid})` : "" })
    : t("router.stopped");
}

function renderConnectPanel() {
  els.baseUrl.textContent = `${baseUrl}/v1`;
  renderAuthToggle();
  renderAllowLan();
}

function renderAuthToggle() {
  const noAuth = Boolean(config.no_auth);
  els.noAuthToggle.checked = noAuth;
  els.tokenModule.hidden = noAuth;
}

function renderAllowLan() {
  const on = Boolean(config.allow_lan);
  els.allowLanToggle.checked = on;
  els.lanBlock.hidden = !on;
  if (on) renderLanUrls();
}

function renderLanUrls() {
  const port = portOf(baseUrl);
  const urls = (lanAddrs || []).map((ip) => `http://${ip}:${port}/v1`);
  els.lanUrls.innerHTML = urls.length
    ? urls
        .map(
          (u) => `
          <div class="kv">
            <code>${esc(u)}</code>
            <button class="ghost" data-copy-url="${esc(u)}">${esc(t("actions.copy"))}</button>
          </div>`,
        )
        .join("")
    : `<span class="muted">${esc(t("connect.noLanAddress"))}</span>`;
}

function channelRowHtml(entry, stateMap) {
  const ch = entry.ch;
  const index = entry.index;
  const st = stateMap[ch.id] || {};
  const label = statusLabel(st.status);
  const models = (ch.models || []).map(esc).join(", ");
  const latency = formatLatency(st.response_time_ms);
  const enabled = ch.enabled !== false;
  return `
    <tr>
      <td class="channel-status-cell">${channelStatusHtml(st, label)}</td>
      <td>${esc(ch.name || ch.id)}<div class="sub">${esc(ch.id || "")}</div>${cooldownReasonHtml(st)}</td>
      <td class="mono">${esc(ch.base_url)}</td>
      <td>${ch.priority ?? 0}</td>
      <td>${esc(ch.group || "default")}</td>
      <td class="channel-latency-cell">${latency}</td>
      <td class="mono">${models || "—"}</td>
      <td class="actions-cell channel-actions-cell">
        <span class="row-action-menu channel-action-menu">
          <button class="mini icon-button" type="button" title="${esc(t("channels.settings"))}" aria-label="${esc(t("channels.settings"))}" data-action="toggle-channel-menu" data-index="${index}">⚙</button>
          <span class="action-menu" popover="auto" data-channel-menu="${index}">
            <button type="button" data-action="edit-channel" data-index="${index}">${esc(t("actions.edit"))}</button>
            <button type="button" data-action="toggle-channel" data-index="${index}">${esc(t(enabled ? "actions.disable" : "actions.enable"))}</button>
            <button type="button" class="danger" data-action="delete-channel" data-index="${index}">${esc(t("actions.delete"))}</button>
          </span>
        </span>
      </td>
    </tr>`;
}

function formatLatency(milliseconds) {
  const ms = Number(milliseconds) || 0;
  if (ms <= 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}

function formatCooldownRemaining(seconds) {
  const s = Math.max(0, Math.ceil(Number(seconds) || 0));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rem = s % 60;
  if (m < 60) return rem ? `${m}m${rem}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const mm = m % 60;
  return mm ? `${h}h${mm}m` : `${h}h`;
}

function cooldownProgressHtml(st) {
  const until = Number(st.cooldown_until) || 0;
  const now = Math.floor(Date.now() / 1000);
  if (!until || until <= now) return "";
  const total = Number(st.cooldown_duration_seconds) || 0;
  if (total <= 0) return "";
  const remaining = Math.max(0, until - now);
  const fraction = Math.max(0, Math.min(1, remaining / total));
  const circumference = 2 * Math.PI * 7;
  const offset = circumference * (1 - fraction);
  const label = formatCooldownRemaining(remaining);
  const title = cooldownTitle(st, label);
  return `
    <span class="cooldown-ring" title="${esc(title)}">
      <svg viewBox="0 0 18 18" aria-hidden="true">
        <g transform="rotate(-90 9 9)">
          <circle class="cooldown-bg" cx="9" cy="9" r="7"></circle>
          <circle class="cooldown-fg" cx="9" cy="9" r="7" style="stroke-dasharray:${circumference.toFixed(2)};stroke-dashoffset:${offset.toFixed(2)}"></circle>
        </g>
      </svg>
      <span class="cooldown-text">${esc(label)}</span>
    </span>`;
}

function cooldownReason(st) {
  const until = Number(st.cooldown_until) || 0;
  if (!until || until <= Math.floor(Date.now() / 1000)) return "";
  return (st.cooldown_reason || "").trim();
}

function cooldownTitle(st, label) {
  const remaining = t("channels.cooldownRemaining", { time: label });
  const reason = cooldownReason(st);
  return reason ? `${remaining} · ${t("channels.cooldownReason", { reason })}` : remaining;
}

// A cooling channel explains itself in the list: the reason is truncated by CSS
// and the full text stays available through the title attribute.
function cooldownReasonHtml(st) {
  const reason = cooldownReason(st);
  if (!reason) return "";
  return `<div class="sub cooldown-reason" title="${esc(reason)}">${esc(reason)}</div>`;
}

function channelStatusHtml(st, label) {
  const badge = `<span class="badge ${esc(st.status || "unknown")}">${esc(label)}</span>`;
  return `<span class="channel-status">${badge}${cooldownProgressHtml(st)}</span>`;
}

function renderAbout() {
  if (!els.aboutCodexVersion) return;
  els.aboutCodexVersion.textContent = codexClientVersion || "—";
}

function renderChannels() {
  // Keep an open action menu stable while the periodic refresh updates data.
  // Replacing the table HTML would remove the popover and close it.
  if (document.querySelector("[data-channel-menu]:popover-open")) return;

  const stateMap = {};
  (channelsState || []).forEach((c) => {
    stateMap[c.id] = c;
  });

  const groups = config.groups && config.groups.length ? config.groups : [{ name: "default", priority: 0 }];
  const groupByName = Object.fromEntries(groups.map((g) => [g.name, g]));
  const buckets = {};
  (config.channels || []).forEach((ch, index) => {
    let g = ch.group || "default";
    if (!groupByName[g]) g = "default";
    (buckets[g] ||= []).push({ ch, index });
  });

  const ordered = [...groups].sort(
    (a, b) => (b.priority ?? 0) - (a.priority ?? 0) || a.name.localeCompare(b.name),
  );

  let html = "";
  for (const g of ordered) {
    const list = buckets[g.name] || [];
    if (!list.length) continue;
    html += `
      <div class="group-block">
        <div class="group-block-title">${esc(g.name)} <span class="muted">${esc(t("groups.priority", { value: g.priority ?? 0 }))}</span></div>
        <div class="table-wrap">
          <table class="channel-table">
            <colgroup>
              <col class="channel-col-status" />
              <col span="7" />
            </colgroup>
            <thead>
              <tr>
                <th class="channel-status-cell">${esc(t("columns.status"))}</th>
                <th>${esc(t("columns.channel"))}</th>
                <th>Base URL</th>
                <th>${esc(t("columns.priority"))}</th>
                <th>${esc(t("columns.group"))}</th>
                <th class="channel-latency-cell">${esc(t("columns.latency"))}</th>
                <th>${esc(t("columns.model"))}</th>
                <th class="channel-actions-cell">${esc(t("columns.actions"))}</th>
              </tr>
            </thead>
            <tbody>${list.map((entry) => channelRowHtml(entry, stateMap)).join("")}</tbody>
          </table>
        </div>
      </div>`;
  }

  els.channelGroups.innerHTML = html;
  els.channelEmpty.hidden = (config.channels || []).length > 0;
}

function renderMappings() {
  const rows = (config.model_mappings || [])
    .map((m, index) => {
      const enabled = m.enabled !== false;
      return `
        <tr>
          <td class="mono">${esc(m.platform_model)}</td>
          <td class="mono">${esc(m.upstream_model)}</td>
          <td>${esc(mappingChannelLabel(m.channel_id))}</td>
          <td><span class="badge ${enabled ? "healthy" : "disabled"}">${esc(t(enabled ? "common.enabled" : "status.disabled"))}</span></td>
          <td class="actions-cell">
            <button class="mini" data-action="edit-mapping" data-index="${index}">${esc(t("actions.edit"))}</button>
            <button class="mini" data-action="toggle-mapping" data-index="${index}">${esc(t(enabled ? "actions.disable" : "actions.enable"))}</button>
            <button class="mini danger" data-action="delete-mapping" data-index="${index}">${esc(t("actions.delete"))}</button>
          </td>
        </tr>`;
    })
    .join("");
  els.mappingRows.innerHTML = rows;
  els.mappingEmpty.hidden = (config.model_mappings || []).length > 0;
}

function maskToken(token) {
  if (!token) return "—";
  if (token.length <= 12) return token;
  return `${token.slice(0, 6)}…${token.slice(-6)}`;
}

function formatTime(ts) {
  if (!ts) return "—";
  return new Date(ts * 1000).toLocaleString(getLocale());
}

function formatCompactNumber(value) {
  const number = Number(value) || 0;
  const units = [
    [1e12, "T"],
    [1e9, "G"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (const [divisor, suffix] of units) {
    if (Math.abs(number) < divisor) continue;
    const scaled = number / divisor;
    const digits = Math.abs(scaled) >= 100 ? 0 : Math.abs(scaled) >= 10 ? 1 : 2;
    return `${scaled.toFixed(digits).replace(/\.0+$|(\.\d*[1-9])0+$/, "$1")}${suffix}`;
  }
  return String(number);
}

function compactNumberHtml(value) {
  const number = Number(value) || 0;
  return `<span title="${esc(number.toLocaleString(getLocale()))}">${esc(formatCompactNumber(number))}</span>`;
}

function durationHtml(value) {
  const milliseconds = Number(value) || 0;
  if (milliseconds <= 0) return "—";
  const seconds = milliseconds / 1000;
  const digits = seconds >= 100 ? 0 : seconds >= 10 ? 1 : 2;
  const display = seconds.toFixed(digits).replace(/\.0+$|(\.\d*[1-9])0+$/, "$1");
  return `<span title="${esc(milliseconds.toLocaleString(getLocale()))} ms">${esc(display)} s</span>`;
}

function portOf(url) {
  try {
    return new URL(url).port || "8787";
  } catch {
    return "8787";
  }
}

function renderTokens() {
  if (document.querySelector("[data-token-menu]:popover-open")) return;

  const rows = (tokens || [])
    .map((token) => {
      const enabled = token.enabled !== false;
      return `
        <tr>
          <td>${esc(token.name)}</td>
          <td class="mono">${esc(maskToken(token.token))}</td>
          <td><span class="badge ${enabled ? "healthy" : "disabled"}">${esc(t(enabled ? "common.enabled" : "status.disabled"))}</span></td>
          <td>${token.request_count ?? 0}</td>
          <td>${compactNumberHtml(token.prompt_tokens)}</td>
          <td>${compactNumberHtml(token.completion_tokens)}</td>
          <td>${formatTime(token.last_used_at)}</td>
          <td class="actions-cell">
            <button class="mini" data-action="copy-token" data-id="${esc(token.id)}">${esc(t("actions.copy"))}</button>
            <button class="mini" data-action="toggle-token" data-id="${esc(token.id)}">${esc(t(enabled ? "actions.disable" : "actions.enable"))}</button>
            <span class="row-action-menu token-action-menu">
              <button class="mini icon-button" type="button" title="${esc(t("tokens.settings"))}" aria-label="${esc(t("tokens.settings"))}" data-action="toggle-token-menu" data-id="${esc(token.id)}">⚙</button>
              <span class="action-menu" popover="auto" data-token-menu="${esc(token.id)}">
                <button type="button" data-action="edit-token" data-id="${esc(token.id)}">${esc(t("actions.edit"))}</button>
                <button type="button" class="danger" data-action="delete-token" data-id="${esc(token.id)}">${esc(t("actions.delete"))}</button>
              </span>
            </span>
          </td>
        </tr>`;
    })
    .join("");
  els.tokenRows.innerHTML = rows;
}

function renderGroups() {
  const groups = config.groups && config.groups.length ? config.groups : [{ name: "default", priority: 0 }];
  els.groupList.innerHTML = groups
    .map(
      (g, index) => `
        <div class="group-row">
          <span class="group-name">${esc(g.name)}${g.name === "default" ? ` <span class="muted">(${esc(t("groups.default"))})</span>` : ""}</span>
          <label class="group-priority">${esc(t("columns.priority"))}
            <input type="number" data-action="group-priority" data-index="${index}" value="${g.priority ?? 0}" />
          </label>
          ${g.name === "default" ? "" : `<button class="mini danger" data-action="delete-group" data-index="${index}">${esc(t("actions.delete"))}</button>`}
        </div>`,
    )
    .join("");
}

function usageSummaryHtml(s) {
  if (!s) return "";
  const blocks = [
    [t("columns.requests"), s.request_count ?? 0, false],
    [t("usage.success"), s.success_count ?? 0, false],
    [t("usage.failed"), s.fail_count ?? 0, false],
    [t("usage.promptTokens"), s.prompt_tokens ?? 0, true],
    [t("usage.completionTokens"), s.completion_tokens ?? 0, true],
  ];
  return blocks
    .map(
      ([label, value, compact]) =>
        `<div class="stat"><span>${label}</span><strong>${compact ? compactNumberHtml(value) : value}</strong></div>`,
    )
    .join("");
}

function usageRowHtml(r) {
  const model = r.upstream_model || r.model || "—";
  const status = r.success
    ? `<span class="badge healthy">${esc(t("status.success"))}</span>`
    : `<span class="badge unhealthy">${esc(t("status.failed"))}</span>`;
  return `
    <tr>
      <td>${esc(formatTime(r.time))}</td>
      <td class="mono">${esc(model)}</td>
      <td>${esc(r.channel_name || r.channel_id || "—")}</td>
      <td>${esc(r.token_name || "—")}</td>
      <td>${compactNumberHtml(r.prompt_tokens)}</td>
      <td>${compactNumberHtml(r.completion_tokens)}</td>
      <td>${durationHtml(r.elapsed_ms)}</td>
      <td>${status}</td>
    </tr>`;
}

async function loadUsage() {
  if (usageLoading) return;
  usageLoading = true;
  try {
    const res = await fetch(`${baseUrl}/api/usage`);
    const data = await res.json();
    const records = data.records || [];
    lastUsageData = data;
    els.usageSummary.innerHTML = usageSummaryHtml(data.summary);
    els.usageRows.innerHTML = records.map(usageRowHtml).join("");
    els.usageEmpty.hidden = records.length > 0;
  } catch {
    lastUsageData = null;
    els.usageSummary.innerHTML = "";
    els.usageRows.innerHTML = "";
    els.usageEmpty.hidden = false;
  } finally {
    usageLoading = false;
  }
}

function scheduleUsageRefresh() {
  clearTimeout(usageRefreshTimer);
  usageRefreshTimer = setTimeout(loadUsage, 1000);
}

function stopUsageUpdates() {
  clearTimeout(usageRefreshTimer);
  usageRefreshTimer = null;
  usageEventSource?.close();
  usageEventSource = null;
}

function startUsageUpdates() {
  stopUsageUpdates();
  loadUsage();
  usageEventSource = new EventSource(`${baseUrl}/api/usage/events`);
  usageEventSource.addEventListener("usage", scheduleUsageRefresh);
}

function usageTabActive() {
  return document.querySelector('.tab.active')?.dataset.tab === "usage";
}

function codexTabActive() {
  return document.querySelector('.tab.active')?.dataset.tab === "tools";
}

function formatBytes(bytes) {
  const value = Number(bytes) || 0;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let scaled = value;
  let unitIndex = 0;
  while (scaled >= 1024 && unitIndex < units.length - 1) {
    scaled /= 1024;
    unitIndex += 1;
  }
  if (unitIndex === 0) return `${value} B`;
  const digits = scaled >= 10 ? 1 : 2;
  return `${scaled.toFixed(digits).replace(/\.0$/, "")} ${units[unitIndex]}`;
}

function codexOverviewHtml(status) {
  const sqlite = status.sqlite_counts;
  const sqliteSessions = sqlite
    ? Object.entries(sqlite.sessions || {}).map(([provider, count]) => `${esc(provider)}: ${count}`).join(" · ") || "—"
    : esc(t("tools.codexNoDb"));
  const sqliteArchived = sqlite
    ? Object.entries(sqlite.archived_sessions || {}).map(([provider, count]) => `${esc(provider)}: ${count}`).join(" · ") || "—"
    : esc(t("tools.codexNoDb"));
  const rolloutSessions = Object.entries(status.rollout_counts?.sessions || {}).map(([provider, count]) => `${esc(provider)}: ${count}`).join(" · ") || "—";
  const rolloutArchived = Object.entries(status.rollout_counts?.archived_sessions || {}).map(([provider, count]) => `${esc(provider)}: ${count}`).join(" · ") || "—";

  return `
    <div class="kv"><span>${esc(t("tools.codexHome"))}</span><code>${esc(status.codex_home)}</code></div>
    <div class="kv"><span>${esc(t("tools.codexCurrentProvider"))}</span><code>${esc(status.current_provider)}${status.current_provider_implicit ? ` <span class="muted">(${esc(t("tools.codexImplicit"))})</span>` : ""}</code></div>
    <div class="kv"><span>${esc(t("tools.codexRollout"))} · sessions</span><span>${rolloutSessions}</span></div>
    <div class="kv"><span>${esc(t("tools.codexRollout"))} · archived</span><span>${rolloutArchived}</span></div>
    <div class="kv"><span>${esc(t("tools.codexSqlite"))} · sessions</span><span>${sqliteSessions}</span></div>
    <div class="kv"><span>${esc(t("tools.codexSqlite"))} · archived</span><span>${sqliteArchived}</span></div>
    <div class="kv"><span>${esc(t("tools.codexBackupSummary"))}</span><span>${status.backup_summary.count} · ${formatBytes(status.backup_summary.total_bytes)}</span></div>`;
}

function renderCodexProviders() {
  const status = codexStatus;
  const providers = status?.configured_providers || [];
  const current = status?.current_provider || "";
  if (!providers.length) {
    els.codexProviderSelect.innerHTML = `<option value="">${esc(t("tools.codexUnknown"))}</option>`;
    return;
  }
  els.codexProviderSelect.innerHTML = providers
    .map((provider) => `<option value="${esc(provider)}" ${provider === current ? "selected" : ""}>${esc(provider)}</option>`)
    .join("");
}

function renderCodexBackups() {
  const rows = (codexBackups || [])
    .map(
      (backup) => `
        <tr>
          <td class="mono">${esc(backup.name)}</td>
          <td>${formatBytes(backup.size)}</td>
          <td class="actions-cell">
            <button class="mini" data-action="codex-restore" data-path="${esc(backup.path)}">${esc(t("tools.codexRestore"))}</button>
          </td>
        </tr>`,
    )
    .join("");
  els.codexBackupRows.innerHTML = rows;
  els.codexBackupEmpty.hidden = (codexBackups || []).length > 0;
}

function renderCodex() {
  renderCodexProviders();
  els.codexOverview.innerHTML = codexStatus
    ? codexOverviewHtml(codexStatus)
    : `<span class="muted">${esc(t("common.routerUnavailable"))}</span>`;
  renderCodexBackups();
}

function openCodexTool() {
  els.codexToolOverlay.classList.remove("hidden");
  loadCodex();
}

function closeCodexTool() {
  els.codexToolOverlay.classList.add("hidden");
}

async function loadCodex() {
  try {
    const [statusRes, backupRes] = await Promise.all([
      fetch(`${baseUrl}/api/codex/status`),
      fetch(`${baseUrl}/api/codex/backups`),
    ]);
    if (!statusRes.ok) {
      const data = await statusRes.json().catch(() => ({}));
      throw new Error(data.error || statusRes.status);
    }
    codexStatus = await statusRes.json();
    const backupData = backupRes.ok ? await backupRes.json() : { backups: [] };
    codexBackups = backupData.backups || [];
    els.codexState.textContent = "";
  } catch (error) {
    codexStatus = null;
    codexBackups = [];
    els.codexState.textContent = t("tools.codexSyncFailed", { error: error.message });
  }
  renderCodex();
}

function openTerminal(title) {
  if (title) els.terminalTitle.textContent = title;
  els.terminalOutput.textContent = "";
  els.terminalOverlay.classList.remove("hidden");
}

function appendTerminalLine(line) {
  els.terminalOutput.textContent +=
    (els.terminalOutput.textContent ? "\n" : "") + line;
  els.terminalOutput.scrollTop = els.terminalOutput.scrollHeight;
}

function stopTerminalStream() {
  terminalEventSource?.close();
  terminalEventSource = null;
}

function closeTerminal() {
  stopTerminalStream();
  els.terminalOverlay.classList.add("hidden");
}

function streamCommand(url, { onResult, onFail, onError } = {}) {
  stopTerminalStream();
  terminalEventSource = new EventSource(url);
  terminalEventSource.addEventListener("progress", (event) => {
    try {
      const data = JSON.parse(event.data);
      if (data.line) appendTerminalLine(data.line);
    } catch {
      // Ignore malformed progress frames.
    }
  });
  terminalEventSource.addEventListener("failed", (event) => {
    let message = "";
    try {
      message = JSON.parse(event.data).error || "";
    } catch {
      // Keep the empty message.
    }
    if (onFail) onFail(message);
    else if (message) appendTerminalLine(message);
    stopTerminalStream();
  });
  terminalEventSource.addEventListener("result", async (event) => {
    stopTerminalStream();
    if (onResult) await onResult(event);
  });
  terminalEventSource.onerror = () => {
    stopTerminalStream();
    if (onError) onError();
  };
}

function runCodexSync() {
  if (codexBusy) return;
  codexBusy = true;
  const provider = els.codexProviderSelect.value || "";
  const current = codexStatus?.current_provider || "";
  const keep = parseInt(els.codexKeepInput.value || "5", 10);
  const restorePinned = els.codexRestorePinned.checked;
  const useSwitch = Boolean(provider && provider !== current);

  const params = new URLSearchParams({
    keep_count: String(keep),
    restore_pinned_projects: restorePinned ? "1" : "0",
    switch: useSwitch ? "1" : "0",
  });
  if (provider) params.set("provider", provider);

  openTerminal(t("tools.codexConsoleTitle"));
  streamCommand(`${baseUrl}/api/codex/sync/events?${params.toString()}`, {
    onFail: () => {
      codexBusy = false;
    },
    onResult: async () => {
      codexBusy = false;
      await loadCodex();
    },
    onError: () => {
      codexBusy = false;
    },
  });
}

async function runCodexPrune() {
  const keep = parseInt(els.codexKeepInput.value || "5", 10);
  els.codexState.textContent = t("tools.codexPruning");
  try {
    const res = await fetch(`${baseUrl}/api/codex/prune`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ keep_count: keep }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.status);
    els.codexState.textContent = t("tools.codexPruned");
    toast(t("tools.codexPruned"));
  } catch (error) {
    els.codexState.textContent = t("tools.codexPruneFailed", { error: error.message });
    toast(t("tools.codexPruneFailed", { error: error.message }));
  } finally {
    await loadCodex();
  }
}

async function restoreCodexBackup(path) {
  if (!path || codexBusy) return;
  codexBusy = true;
  els.codexState.textContent = t("tools.codexRestoring");
  try {
    const res = await fetch(`${baseUrl}/api/codex/restore`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ backup_dir: path }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.status);
    els.codexState.textContent = t("tools.codexRestored");
    toast(t("tools.codexRestored"));
  } catch (error) {
    els.codexState.textContent = t("tools.codexRestoreFailed", { error: error.message });
    toast(t("tools.codexRestoreFailed", { error: error.message }));
  } finally {
    codexBusy = false;
    await loadCodex();
  }
}

async function refresh() {
  const processStatus = await safeInvoke("router_status");
  renderProcess(processStatus);

  const resolvedBase = await safeInvoke("get_router_base_url");
  if (resolvedBase) baseUrl = resolvedBase;

  try {
    const [stateRes, configRes, tokenRes] = await Promise.all([
      fetch(`${baseUrl}/api/state`),
      fetch(`${baseUrl}/api/config`),
      fetch(`${baseUrl}/api/tokens`),
    ]);
    if (!stateRes.ok || !configRes.ok || !tokenRes.ok) throw new Error("upstream not ready");

    const state = await stateRes.json();
    const configData = await configRes.json();
    const tokenData = await tokenRes.json();

    config = configData;
    config.channels ||= [];
    config.model_mappings ||= [];
    config.groups ||= [{ name: "default", priority: 0 }];
    channelsState = state.channels || [];
    lanAddrs = state.lan_addrs || [];
    tokens = tokenData.tokens || [];
    codexClientVersion = state.codex_client_version || "";

    renderConnectPanel();
    renderChannels();
    renderMappings();
    renderGroups();
    renderTokens();
    renderAbout();
    els.lastUpdated.textContent = t("common.updatedAt", { time: new Date().toLocaleTimeString(getLocale()) });
  } catch {
    els.lastUpdated.textContent = t("common.routerUnavailable");
  }
}

async function saveConfig() {
  const res = await fetch(`${baseUrl}/api/config`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    toast(t("common.saveFailed", { error: data.error || res.status }));
    return false;
  }
  toast(t("common.saved"));
  return true;
}

function closeModal() {
  modalType = null;
  modalId = null;
  els.modal.classList.add("hidden");
}

function groupOptionsHtml(selected) {
  const groups = config.groups && config.groups.length ? config.groups : [{ name: "default", priority: 0 }];
  const current = selected || "default";
  return groups
    .map((g) => `<option value="${esc(g.name)}" ${g.name === current ? "selected" : ""}>${esc(g.name)}</option>`)
    .join("");
}

function channelOptionsHtml(selected) {
  const current = selected || "";
  const options = (config.channels || []).map((channel) => {
    const label = `${channel.name || channel.id} (${channel.id})`;
    return `<option value="${esc(channel.id)}" ${channel.id === current ? "selected" : ""}>${esc(label)}</option>`;
  });
  return `<option value="" ${current ? "" : "selected"}>${esc(t("mappings.supportedChannels"))}</option>${options.join("")}`;
}

function mappingChannelLabel(channelID) {
  if (!channelID) return t("mappings.supportedChannels");
  const channel = (config.channels || []).find((item) => item.id === channelID);
  return channel ? `${channel.name || channel.id} (${channel.id})` : channelID;
}

function channelAuthMethod(ch) {
  if (ch?.codex_auth_method) return ch.codex_auth_method;
  if (ch?.auth_type === "none") return "none";
  if (ch?.auth_type !== "codex" && !ch?.codex_auth) return "api_key";
  switch (ch?.codex_auth?.auth_mode) {
    case "oauth":
      return "codex_oauth";
    case "refresh_token":
      return "codex_refresh_token";
    case "personal_access_token":
      return "codex_pat";
    default:
      return "codex_json";
  }
}

function formatAuthExpiry(value) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString(getLocale() === "zh-CN" ? "zh-CN" : "en-US");
}

function codexAuthSummaryHtml(ch) {
  const auth = ch?.codex_auth;
  if (!auth) return "";
  const refreshable = Boolean(auth.refresh_token);
  const details = [
    auth.email ? `${t("channels.authAccount")}: ${auth.email}` : "",
    auth.expires_at ? `${t("channels.authExpires")}: ${formatAuthExpiry(auth.expires_at)}` : "",
  ].filter(Boolean).join(" · ");
  return `
    <div class="auth-summary">
      <div>${esc(details || t("channels.authConfigured"))}</div>
      ${refreshable ? `<button type="button" class="mini" data-action="refresh-codex-auth">${esc(t("channels.refreshAuth"))}</button>` : ""}
    </div>`;
}

function channelFormHtml(ch) {
  ch = ch || {};
  const authMethod = channelAuthMethod(ch);
  const isCodex = authMethod.startsWith("codex_");
  const codexAuthInput = ch.codex_auth_input ?? codexAuthJSON(ch.codex_auth);
  const baseURL = ch.base_url || (isCodex ? "https://chatgpt.com/backend-api/codex" : "");
  const placement = ch.api_key_placement || "bearer";
  const showAPIKeyAdvanced = placement !== "bearer"
    || (ch.api_key_prefix != null && ch.api_key_prefix !== "Bearer");
  const oauthURL = ch.codex_oauth_url || "";
  return `
    <div class="form-section">
      <div class="form-section-title">${esc(t("channels.basic"))}</div>
      <label>${esc(t("columns.name"))}<input name="name" value="${esc(ch.name || "")}" placeholder="${esc(t("channels.namePlaceholder"))}"></label>
      <label>${esc(t("channels.authType"))}
        <select name="auth_method">
          <option value="api_key" ${authMethod === "api_key" ? "selected" : ""}>${esc(t("channels.authApiKey"))}</option>
          <option value="none" ${authMethod === "none" ? "selected" : ""}>${esc(t("channels.authNone"))}</option>
          <option value="codex_oauth" ${authMethod === "codex_oauth" ? "selected" : ""}>${esc(t("channels.authCodexOAuth"))}</option>
          <option value="codex_json" ${authMethod === "codex_json" ? "selected" : ""}>${esc(t("channels.authCodex"))}</option>
          <option value="codex_refresh_token" ${authMethod === "codex_refresh_token" ? "selected" : ""}>${esc(t("channels.authCodexRT"))}</option>
          <option value="codex_pat" ${authMethod === "codex_pat" ? "selected" : ""}>${esc(t("channels.authCodexPAT"))}</option>
        </select>
      </label>
      <label>Base URL *<input name="base_url" value="${esc(baseURL)}" placeholder="https://api.deepseek.com"></label>
      <div data-auth-panel="api_key" ${authMethod === "api_key" ? "" : "hidden"}>
        <label>API Key<input name="api_key" value="${esc(ch.api_key || "")}" placeholder="sk-..."></label>
        <details class="api-key-advanced" ${showAPIKeyAdvanced ? "open" : ""}>
          <summary>
            <span>${esc(t("channels.apiKeyAdvanced"))}</span>
            <span>${esc(t("channels.apiKeyAdvancedHint"))}</span>
          </summary>
          <div class="api-key-advanced-body">
            <div class="grid">
              <label>${esc(t("channels.apiKeyPlacement"))}
                <select name="api_key_placement">
                  <option value="bearer" ${placement === "bearer" ? "selected" : ""}>${esc(t("channels.apiKeyBearer"))}</option>
                  <option value="authorization" ${placement === "authorization" ? "selected" : ""}>${esc(t("channels.apiKeyAuthorization"))}</option>
                  <option value="header" ${placement === "header" ? "selected" : ""}>${esc(t("channels.apiKeyHeader"))}</option>
                  <option value="query" ${placement === "query" ? "selected" : ""}>${esc(t("channels.apiKeyQuery"))}</option>
                </select>
              </label>
              <label data-api-key-field="prefix">${esc(t("channels.apiKeyPrefix"))}<input name="api_key_prefix" value="${esc(ch.api_key_prefix ?? (placement === "bearer" ? "Bearer" : ""))}" placeholder="Bearer"></label>
              <label data-api-key-field="header">${esc(t("channels.apiKeyHeaderName"))}<input name="api_key_header" value="${esc(ch.api_key_header || "X-API-Key")}" placeholder="X-API-Key"></label>
              <label data-api-key-field="query">${esc(t("channels.apiKeyQueryName"))}<input name="api_key_query" value="${esc(ch.api_key_query || "api_key")}" placeholder="api_key"></label>
            </div>
            <small class="field-hint">${esc(t("channels.apiKeyPlacementHint"))}</small>
          </div>
        </details>
      </div>
      <div data-auth-panel="none" ${authMethod === "none" ? "" : "hidden"}>
        <div class="auth-note">${esc(t("channels.authNoneHint"))}</div>
      </div>
      <div data-auth-panel="codex_oauth" ${authMethod === "codex_oauth" ? "" : "hidden"}>
        ${codexAuthSummaryHtml(ch)}
        <div class="auth-note">${esc(t("channels.codexOAuthHint"))}</div>
        <div class="oauth-actions">
          <button type="button" class="mini" data-action="start-codex-oauth">${esc(t("channels.generateAuthURL"))}</button>
          <button type="button" class="mini" data-action="open-codex-oauth" ${oauthURL ? "" : "disabled"}>${esc(t("channels.openAuthURL"))}</button>
          <button type="button" class="mini" data-action="copy-codex-oauth" ${oauthURL ? "" : "disabled"}>${esc(t("actions.copy"))}</button>
        </div>
        <input type="hidden" name="codex_oauth_session" value="${esc(ch.codex_oauth_session || "")}">
        <input type="hidden" name="codex_oauth_state" value="${esc(ch.codex_oauth_state || "")}">
        <textarea name="codex_oauth_url" rows="3" readonly placeholder="${esc(t("channels.authURLPlaceholder"))}">${esc(oauthURL)}</textarea>
        <label>${esc(t("channels.oauthCallback"))}
          <textarea name="codex_oauth_callback" rows="4" placeholder="${esc(t("channels.oauthCallbackPlaceholder"))}">${esc(ch.codex_oauth_callback || "")}</textarea>
          <small class="field-hint">${esc(t("channels.oauthCallbackHint"))}</small>
        </label>
      </div>
      <div data-auth-panel="codex_json" ${authMethod === "codex_json" ? "" : "hidden"}>
        ${codexAuthSummaryHtml(ch)}
        <label>${esc(t("channels.codexJson"))}
          <textarea name="codex_auth_input" rows="8" placeholder='{"tokens":{"access_token":"...","refresh_token":"..."}}'>${esc(codexAuthInput)}</textarea>
          <small class="field-hint">${esc(t("channels.codexJsonHint"))}</small>
        </label>
      </div>
      <div data-auth-panel="codex_refresh_token" ${authMethod === "codex_refresh_token" ? "" : "hidden"}>
        ${codexAuthSummaryHtml(ch)}
        <label>${esc(t("channels.codexRefreshToken"))}
          <textarea name="codex_refresh_token" rows="4" placeholder="${esc(t("channels.codexRefreshTokenPlaceholder"))}">${esc(ch.codex_refresh_token || "")}</textarea>
          <small class="field-hint">${esc(t("channels.codexRefreshTokenHint"))}</small>
        </label>
        <label>${esc(t("channels.codexClientID"))}<input name="codex_client_id" value="${esc(ch.codex_client_id || ch.codex_auth?.client_id || "")}" placeholder="app_..."></label>
      </div>
      <div data-auth-panel="codex_pat" ${authMethod === "codex_pat" ? "" : "hidden"}>
        ${codexAuthSummaryHtml(ch)}
        <label>${esc(t("channels.codexPAT"))}
          <input name="codex_pat" value="${esc(ch.codex_pat || "")}" placeholder="at-...">
          <small class="field-hint">${esc(t("channels.codexPATHint"))}</small>
        </label>
      </div>
    </div>
    <div class="form-section">
      <div class="form-section-title">${esc(t("channels.models"))}</div>
      <div class="field">
        <div class="field-head">
          <span class="field-title">${esc(t("channels.availableModels"))}</span>
          <button type="button" class="mini" data-action="fetch-models" ${isCodex && !ch.id ? "hidden" : ""}>${esc(t("actions.update"))}</button>
        </div>
        <textarea name="models" rows="4" placeholder="${isCodex ? "*" : "deepseek-chat"}">${esc((ch.models || (isCodex ? ["*"] : [])).join("\n"))}</textarea>
        <small class="field-hint" data-models-hint>${esc(t(isCodex ? "channels.codexModelsHint" : "channels.modelsHint"))}</small>
      </div>
    </div>
    <div class="form-section">
      <div class="form-section-title">${esc(t("channels.routing"))}</div>
      <div class="grid">
        <label>${esc(t("columns.priority"))}<input name="priority" type="number" value="${ch.priority ?? 0}"><small class="field-hint">${esc(t("channels.priorityHint"))}</small></label>
        <label>${esc(t("columns.group"))}<select name="group">${groupOptionsHtml(ch.group)}</select><small class="field-hint">${esc(t("channels.groupHint"))}</small></label>
        <label>${esc(t("channels.maxRetries"))}<input name="max_retries" type="number" value="${ch.max_retries ?? 0}"><small class="field-hint">${esc(t("channels.maxRetriesHint"))}</small></label>
      </div>
    </div>
    <label class="check"><input name="enabled" type="checkbox" ${ch.enabled === false ? "" : "checked"}> ${esc(t("channels.enable"))}</label>`;
}

function mappingFormHtml(m) {
  m = m || {};
  return `
    <div class="form-section">
      <div class="form-section-title">${esc(t("mappings.section"))}</div>
      <label>${esc(t("columns.platformModel"))} *<input name="platform_model" value="${esc(m.platform_model || "")}" placeholder="gpt-5.6-sol"><small class="field-hint">${esc(t("mappings.platformHint"))}</small></label>
      <label>${esc(t("columns.upstreamModel"))} *<input name="upstream_model" value="${esc(m.upstream_model || "")}" placeholder="deepseek-v4-pro"><small class="field-hint">${esc(t("mappings.upstreamHint"))}</small></label>
    </div>
    <div class="form-section">
      <div class="form-section-title">${esc(t("mappings.scope"))}</div>
      <label>${esc(t("columns.channel"))}<select name="channel_id">${channelOptionsHtml(m.channel_id)}</select><small class="field-hint">${esc(t("mappings.channelHint"))}</small></label>
    </div>
    <label class="check"><input name="enabled" type="checkbox" ${m.enabled === false ? "" : "checked"}> ${esc(t("common.enabled"))}</label>`;
}

function tokenFormHtml() {
  return `
    <div class="form-section">
      <label>${esc(t("tokens.title"))}<input name="name" placeholder="${esc(t("tokens.namePlaceholder"))}"></label>
    </div>`;
}

function tokenEditFormHtml(token) {
  return `
    <div class="form-section">
      <label>${esc(t("tokens.title"))}<input name="name" value="${esc(token?.name || "")}" placeholder="${esc(t("tokens.namePlaceholder"))}"></label>
    </div>`;
}

function openChannelForm(index) {
  const ch = index >= 0 ? config.channels[index] : null;
  modalType = "channel";
  modalId = index;
  els.modalTitle.textContent = t(ch ? "channels.edit" : "channels.add");
  els.modalBody.innerHTML = channelFormHtml(ch);
  syncChannelAuthPanels();
  els.modal.classList.remove("hidden");
}

function openMappingForm(index) {
  const m = index >= 0 ? config.model_mappings[index] : null;
  modalType = "mapping";
  modalId = index;
  els.modalTitle.textContent = t(m ? "mappings.edit" : "mappings.add");
  els.modalBody.innerHTML = mappingFormHtml(m);
  els.modal.classList.remove("hidden");
}

function openTokenForm() {
  modalType = "token";
  modalId = null;
  els.modalTitle.textContent = t("tokens.generate");
  els.modalBody.innerHTML = tokenFormHtml();
  els.modal.classList.remove("hidden");
}

function openTokenEditForm(id) {
  const token = tokens.find((item) => item.id === id);
  if (!token) return;
  modalType = "token-edit";
  modalId = id;
  els.modalTitle.textContent = t("tokens.edit");
  els.modalBody.innerHTML = tokenEditFormHtml(token);
  els.modal.classList.remove("hidden");
}

function codexAuthJSON(auth) {
  if (!auth) return "";
  return JSON.stringify({
    auth_mode: auth.auth_mode || "",
    tokens: {
      access_token: auth.access_token || "",
      refresh_token: auth.refresh_token || "",
      id_token: auth.id_token || "",
      account_id: auth.account_id || "",
      expires_at: auth.expires_at || "",
    },
    email: auth.email || "",
    user_id: auth.user_id || "",
    client_id: auth.client_id || "",
    updated_at: auth.updated_at || 0,
  }, null, 2);
}

function syncChannelAuthPanels() {
  if (modalType !== "channel") return;
  const authMethod = els.modalBody.querySelector('[name="auth_method"]')?.value || "api_key";
  const isCodex = authMethod.startsWith("codex_");
  els.modalBody.querySelectorAll("[data-auth-panel]").forEach((panel) => {
    panel.hidden = panel.dataset.authPanel !== authMethod;
  });
  const fetchButton = els.modalBody.querySelector('[data-action="fetch-models"]');
  const savedCodex = modalId >= 0 && config.channels?.[modalId]?.auth_type === "codex";
  if (fetchButton) fetchButton.hidden = isCodex && !savedCodex;
  const hint = els.modalBody.querySelector("[data-models-hint]");
  if (hint) hint.textContent = t(isCodex ? "channels.codexModelsHint" : "channels.modelsHint");
  const baseField = els.modalBody.querySelector('[name="base_url"]');
  if (isCodex && baseField && !baseField.value.trim()) {
    baseField.value = "https://chatgpt.com/backend-api/codex";
  }
  const modelsField = els.modalBody.querySelector('[name="models"]');
  if (isCodex && modelsField && !modelsField.value.trim()) {
    modelsField.value = "*";
  }
  syncAPIKeyFields();
}

function syncAPIKeyFields() {
  const placement = els.modalBody.querySelector('[name="api_key_placement"]')?.value || "bearer";
  els.modalBody.querySelectorAll("[data-api-key-field]").forEach((field) => {
    const kind = field.dataset.apiKeyField;
    field.hidden = (kind === "header" && placement !== "header")
      || (kind === "query" && placement !== "query")
      || (kind === "prefix" && placement === "query");
  });
}

function collectChannelDraft() {
  const f = els.modalBody;
  const val = (name) => (f.querySelector(`[name="${name}"]`)?.value || "").trim();
  const models = val("models")
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  const authMethod = val("auth_method") || "api_key";
  return {
    id: modalId >= 0 ? (config.channels[modalId]?.id || "") : "",
    name: val("name"),
    base_url: val("base_url"),
    auth_type: authMethod === "none" ? "none" : (authMethod.startsWith("codex_") ? "codex" : "api_key"),
    codex_auth_method: authMethod,
    api_key: val("api_key"),
    api_key_placement: val("api_key_placement") || "bearer",
    api_key_header: val("api_key_header"),
    api_key_prefix: val("api_key_prefix"),
    api_key_query: val("api_key_query"),
    codex_auth: modalId >= 0 ? config.channels[modalId]?.codex_auth : undefined,
    codex_auth_input: val("codex_auth_input"),
    codex_refresh_token: val("codex_refresh_token"),
    codex_client_id: val("codex_client_id"),
    codex_pat: val("codex_pat"),
    codex_oauth_session: val("codex_oauth_session"),
    codex_oauth_state: val("codex_oauth_state"),
    codex_oauth_url: val("codex_oauth_url"),
    codex_oauth_callback: val("codex_oauth_callback"),
    models,
    model_mappings: modalId >= 0 ? (config.channels[modalId]?.model_mappings || {}) : {},
    priority: parseInt(val("priority") || "0", 10),
    group: f.querySelector('[name="group"]').value,
    max_retries: parseInt(val("max_retries") || "0", 10),
    enabled: f.querySelector('[name="enabled"]').checked,
  };
}

async function collectChannelForm() {
  const ch = collectChannelDraft();
  if (ch.auth_type === "codex") {
    const existingAuth = modalId >= 0 ? config.channels[modalId]?.codex_auth : null;
    try {
      let res;
      if (ch.codex_auth_method === "codex_oauth" && ch.codex_oauth_callback) {
        res = await fetch(`${baseUrl}/api/codex/oauth/exchange`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            session_id: ch.codex_oauth_session,
            state: ch.codex_oauth_state,
            code: ch.codex_oauth_callback,
          }),
        });
      } else if (ch.codex_auth_method === "codex_json" && ch.codex_auth_input) {
        res = await fetch(`${baseUrl}/api/parse_codex_auth`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ content: ch.codex_auth_input }),
        });
      } else if (ch.codex_auth_method === "codex_refresh_token" && ch.codex_refresh_token) {
        res = await fetch(`${baseUrl}/api/codex/refresh-token`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ refresh_token: ch.codex_refresh_token, client_id: ch.codex_client_id }),
        });
      } else if (ch.codex_auth_method === "codex_pat" && ch.codex_pat) {
        res = await fetch(`${baseUrl}/api/codex/pat/validate`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ access_token: ch.codex_pat }),
        });
      } else if (existingAuth && ch.codex_auth_method === channelAuthMethod(config.channels[modalId])) {
        ch.codex_auth = existingAuth;
      } else {
        toast(t("channels.codexCredentialRequired"));
        return null;
      }
      if (!res) {
        ch.codex_auth ||= existingAuth;
      } else {
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          toast(t("channels.codexAuthInvalid", { error: data.error || res.status }));
          return null;
        }
        ch.codex_auth = data.auth;
      }
      ch.api_key = "";
      ch.base_url ||= "https://chatgpt.com/backend-api/codex";
      if (!ch.models.length) ch.models = ["*"];
    } catch {
      toast(t("channels.codexAuthInvalid", { error: t("common.routerUnavailable") }));
      return null;
    }
  } else {
    delete ch.codex_auth;
    if (ch.auth_type === "none") ch.api_key = "";
  }
  delete ch.codex_auth_method;
  delete ch.codex_auth_input;
  delete ch.codex_refresh_token;
  delete ch.codex_client_id;
  delete ch.codex_pat;
  delete ch.codex_oauth_session;
  delete ch.codex_oauth_state;
  delete ch.codex_oauth_url;
  delete ch.codex_oauth_callback;
  return ch;
}

function collectMappingForm() {
  const f = els.modalBody;
  return {
    platform_model: f.querySelector('[name="platform_model"]').value.trim(),
    upstream_model: f.querySelector('[name="upstream_model"]').value.trim(),
    channel_id: f.querySelector('[name="channel_id"]').value.trim(),
    enabled: f.querySelector('[name="enabled"]').checked,
  };
}

async function fetchModelsFromForm() {
  const authMethod = els.modalBody.querySelector('[name="auth_method"]')?.value || "api_key";
  if (authMethod.startsWith("codex_")) {
    const channel = modalId >= 0 ? config.channels?.[modalId] : null;
    if (!channel?.id || channel.auth_type !== "codex") {
      toast(t("channels.saveBeforeFetchModels"));
      return;
    }
    toast(t("channels.fetchingModels"));
    try {
      const res = await fetch(`${baseUrl}/api/probe_models`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ channel_id: channel.id }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(t("channels.fetchFailed", { error: data.error || res.status }));
        return;
      }
      const modelsField = els.modalBody.querySelector('[name="models"]');
      modelsField.value = (data.models || []).join("\n");
      toast(t("channels.fetchSuccess", { count: (data.models || []).length }));
    } catch {
      toast(t("channels.fetchError"));
    }
    return;
  }
  const base = (els.modalBody.querySelector('[name="base_url"]')?.value || "").trim();
  const apiKey = (els.modalBody.querySelector('[name="api_key"]')?.value || "").trim();
  const modelsField = els.modalBody.querySelector('[name="models"]');
  if (!base) {
    toast(t("channels.baseUrlRequired"));
    return;
  }
  toast(t("channels.fetchingModels"));
  try {
    const res = await fetch(`${baseUrl}/api/probe_models`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        base_url: base,
        auth_type: authMethod === "none" ? "none" : "api_key",
        api_key: apiKey,
        api_key_placement: (els.modalBody.querySelector('[name="api_key_placement"]')?.value || "bearer"),
        api_key_header: (els.modalBody.querySelector('[name="api_key_header"]')?.value || "").trim(),
        api_key_prefix: (els.modalBody.querySelector('[name="api_key_prefix"]')?.value || "").trim(),
        api_key_query: (els.modalBody.querySelector('[name="api_key_query"]')?.value || "").trim(),
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      toast(t("channels.fetchFailed", { error: data.error || res.status }));
      return;
    }
    modelsField.value = (data.models || []).join("\n");
    toast(t("channels.fetchSuccess", { count: (data.models || []).length }));
  } catch {
    toast(t("channels.fetchError"));
  }
}

async function startCodexOAuth() {
  toast(t("channels.generatingAuthURL"));
  try {
    const res = await fetch(`${baseUrl}/api/codex/oauth/start`, { method: "POST" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(t("channels.codexAuthInvalid", { error: data.error || res.status }));
      return;
    }
    const set = (name, value) => {
      const field = els.modalBody.querySelector(`[name="${name}"]`);
      if (field) field.value = value || "";
    };
    set("codex_oauth_session", data.session_id);
    set("codex_oauth_state", data.state);
    set("codex_oauth_url", data.auth_url);
    els.modalBody.querySelectorAll('[data-action="open-codex-oauth"], [data-action="copy-codex-oauth"]').forEach((button) => {
      button.disabled = false;
    });
    toast(t("channels.authURLReady"));
    await openExternal(data.auth_url);
  } catch {
    toast(t("channels.codexAuthInvalid", { error: t("common.routerUnavailable") }));
  }
}

async function refreshCodexAuthorization() {
  if (modalId < 0) {
    toast(t("channels.saveBeforeRefresh"));
    return;
  }
  const channel = config.channels?.[modalId];
  if (!channel?.id) return;
  toast(t("channels.refreshingAuth"));
  try {
    const res = await fetch(`${baseUrl}/api/codex/refresh/${encodeURIComponent(channel.id)}`, { method: "POST" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(t("channels.codexAuthInvalid", { error: data.error || res.status }));
      return;
    }
    const draft = collectChannelDraft();
    draft.codex_auth = data.auth;
    config.channels[modalId].codex_auth = data.auth;
    els.modalBody.innerHTML = channelFormHtml(draft);
    syncChannelAuthPanels();
    toast(t("channels.authRefreshed"));
  } catch {
    toast(t("channels.codexAuthInvalid", { error: t("common.routerUnavailable") }));
  }
}

async function saveModal() {
  if (modalType === "channel") {
    const ch = await collectChannelForm();
    if (!ch) return;
    if (!ch.name || !ch.base_url) {
      toast(t("channels.required"));
      return;
    }
    if (modalId >= 0) {
      config.channels[modalId] = ch;
    } else {
      config.channels.push(ch);
    }
    if (await saveConfig()) {
      closeModal();
      refresh();
    }
  } else if (modalType === "mapping") {
    const m = collectMappingForm();
    if (!m.platform_model || !m.upstream_model) {
      toast(t("mappings.required"));
      return;
    }
    if (modalId >= 0) {
      config.model_mappings[modalId] = m;
    } else {
      config.model_mappings.push(m);
    }
    if (await saveConfig()) {
      closeModal();
      refresh();
    }
  } else if (modalType === "token") {
    const name = els.modalBody.querySelector('[name="name"]').value.trim() || "token";
    const res = await fetch(`${baseUrl}/api/tokens`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      toast(t("tokens.generateFailed", { error: data.error || res.status }));
      return;
    }
    const data = await res.json();
    void data;
    closeModal();
    refresh();
    toast(t("tokens.generated"));
  } else if (modalType === "token-edit") {
    const name = els.modalBody.querySelector('[name="name"]')?.value.trim() || "";
    if (!name) {
      toast(t("tokens.nameRequired"));
      return;
    }
    const res = await fetch(`${baseUrl}/api/tokens/${encodeURIComponent(modalId)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      toast(t("common.saveFailed", { error: data.error || res.status }));
      return;
    }
    closeModal();
    refresh();
    toast(t("tokens.updated"));
  }
}

async function addGroup() {
  const name = els.groupInput.value.trim();
  const priority = parseInt(els.groupPriorityInput.value || "0", 10);
  if (!name) {
    toast(t("groups.nameRequired"));
    return;
  }
  if (!config.groups) config.groups = [{ name: "default", priority: 0 }];
  if (config.groups.some((g) => g.name === name)) {
    toast(t("groups.exists"));
    return;
  }
  config.groups.push({ name, priority });
  els.groupInput.value = "";
  els.groupPriorityInput.value = "0";
  if (await saveConfig()) {
    refresh();
  }
}

async function updateGroupPriority(input) {
  const index = Number(input.dataset.index);
  const groups = config.groups || [];
  if (!groups[index]) return;
  groups[index].priority = parseInt(input.value || "0", 10);
  await saveConfig();
}

async function handleAction(action, target) {
  const index = Number(target.dataset.index);
  const id = target.dataset.id;

  if (action === "codex-restore") return restoreCodexBackup(target.dataset.path);
  if (action === "toggle-channel-menu") {
    const menu = document.querySelector(`[data-channel-menu="${index}"]`);
    return toggleActionMenu(menu, target);
  }
  if (action === "edit-channel") {
    document.querySelector(`[data-channel-menu="${index}"]`)?.hidePopover();
    return openChannelForm(index);
  }
  if (action === "edit-mapping") return openMappingForm(index);
  if (action === "fetch-models") return fetchModelsFromForm();
  if (action === "start-codex-oauth") return startCodexOAuth();
  if (action === "open-codex-oauth") {
    return openExternal(els.modalBody.querySelector('[name="codex_oauth_url"]')?.value || "");
  }
  if (action === "copy-codex-oauth") {
    return copyText(els.modalBody.querySelector('[name="codex_oauth_url"]')?.value || "");
  }
  if (action === "refresh-codex-auth") return refreshCodexAuthorization();
  if (action === "delete-group") {
    const idx = Number(target.dataset.index);
    const groups = config.groups || [];
    const g = groups[idx];
    if (g && g.name !== "default") {
      config.groups = groups.filter((x) => x.name !== g.name);
      (config.channels || []).forEach((ch) => {
        if (ch.group === g.name) ch.group = "default";
      });
      await saveConfig();
      refresh();
    }
    return;
  }

  if (action === "toggle-channel") {
    document.querySelector(`[data-channel-menu="${index}"]`)?.hidePopover();
    config.channels[index].enabled = !(config.channels[index].enabled !== false);
    await saveConfig();
    return refresh();
  }
  if (action === "delete-channel") {
    document.querySelector(`[data-channel-menu="${index}"]`)?.hidePopover();
    config.channels.splice(index, 1);
    await saveConfig();
    return refresh();
  }
  if (action === "toggle-mapping") {
    config.model_mappings[index].enabled = !(config.model_mappings[index].enabled !== false);
    await saveConfig();
    return refresh();
  }
  if (action === "delete-mapping") {
    config.model_mappings.splice(index, 1);
    await saveConfig();
    return refresh();
  }

  if (action === "copy-token") {
    const t = tokens.find((x) => x.id === id);
    return copyText(t?.token || "");
  }
  if (action === "toggle-token-menu") {
    const menu = document.querySelector(`[data-token-menu="${CSS.escape(id)}"]`);
    return toggleActionMenu(menu, target);
  }
  if (action === "edit-token") {
    document.querySelector(`[data-token-menu="${CSS.escape(id)}"]`)?.hidePopover();
    return openTokenEditForm(id);
  }
  if (action === "toggle-token") {
    const t = tokens.find((x) => x.id === id);
    await fetch(`${baseUrl}/api/tokens/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled: !(t.enabled !== false) }),
    });
    return refresh();
  }
  if (action === "delete-token") {
    document.querySelector(`[data-token-menu="${CSS.escape(id)}"]`)?.hidePopover();
    await fetch(`${baseUrl}/api/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });
    return refresh();
  }
}

function toggleActionMenu(menu, target) {
  if (!menu) return;
  if (menu.matches(":popover-open")) {
    menu.hidePopover();
    return;
  }
  document.querySelectorAll(".action-menu:popover-open").forEach((item) => item.hidePopover());
  menu.showPopover();
  const buttonRect = target.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  const top = buttonRect.top > menuRect.height + 12
    ? buttonRect.top - menuRect.height - 5
    : buttonRect.bottom + 5;
  menu.style.left = `${Math.max(8, Math.min(buttonRect.right - menuRect.width, window.innerWidth - menuRect.width - 8))}px`;
  menu.style.top = `${top}px`;
}

async function startRouter() {
  toast(t("router.starting"));
  const result = await safeInvoke("start_router");
  toast(t(result?.running ? "router.started" : "router.startFailed"));
  setTimeout(refresh, 600);
  return result;
}

async function stopRouter() {
  toast(t("router.stopping"));
  const result = await safeInvoke("stop_router");
  toast(t(result?.running === false ? "router.stoppedToast" : "router.notRunning"));
  setTimeout(refresh, 300);
  return result;
}

async function checkAll() {
  toast(t("channels.checkStarted"));
  await fetch(`${baseUrl}/api/check`, { method: "POST" });
  setTimeout(refresh, 1200);
}

function switchTab(name) {
  document.querySelectorAll(".tab").forEach((tab) => {
    tab.classList.toggle("active", tab.dataset.tab === name);
  });
  document.querySelectorAll(".panel").forEach((panel) => {
    panel.classList.toggle("active", panel.id === name);
  });
  if (name === "usage" && !document.hidden) {
    startUsageUpdates();
  } else {
    stopUsageUpdates();
  }
  if (name === "tools") {
    loadCodex();
  }
}

function modalDraft() {
  if (els.modal.classList.contains("hidden")) return null;
  if (modalType === "channel") return collectChannelDraft();
  if (modalType === "mapping") return collectMappingForm();
  if (modalType === "token" || modalType === "token-edit") return { name: els.modalBody.querySelector('[name="name"]')?.value || "" };
  return null;
}

function renderLocaleChange(draft) {
  applyTranslations();
  renderProcess(lastProcessStatus);
  renderConnectPanel();
  renderChannels();
  renderMappings();
  renderGroups();
  renderTokens();
  renderCodex();
  if (lastUsageData) {
    const records = lastUsageData.records || [];
    els.usageSummary.innerHTML = usageSummaryHtml(lastUsageData.summary);
    els.usageRows.innerHTML = records.map(usageRowHtml).join("");
  }
  if (modalType === "channel" && draft) {
    els.modalTitle.textContent = t(modalId >= 0 ? "channels.edit" : "channels.add");
    els.modalBody.innerHTML = channelFormHtml(draft);
    syncChannelAuthPanels();
  } else if (modalType === "mapping" && draft) {
    els.modalTitle.textContent = t(modalId >= 0 ? "mappings.edit" : "mappings.add");
    els.modalBody.innerHTML = mappingFormHtml(draft);
  } else if (modalType === "token" && draft) {
    els.modalTitle.textContent = t("tokens.generate");
    els.modalBody.innerHTML = tokenFormHtml();
    els.modalBody.querySelector('[name="name"]').value = draft.name;
  } else if (modalType === "token-edit" && draft) {
    els.modalTitle.textContent = t("tokens.edit");
    els.modalBody.innerHTML = tokenEditFormHtml({ name: draft.name });
  }
}

window.addEventListener("DOMContentLoaded", () => {
  els.toast = document.querySelector("#toast");
  els.routerToggle = document.querySelector("#router-toggle");
  els.processState = document.querySelector("#process-state");
  els.baseUrl = document.querySelector("#base-url");
  els.noAuthToggle = document.querySelector("#no-auth-toggle");
  els.tokenModule = document.querySelector("#token-module");
  els.tokenRows = document.querySelector("#token-rows");
  els.channelGroups = document.querySelector("#channel-groups");
  els.channelEmpty = document.querySelector("#channel-empty");
  els.mappingRows = document.querySelector("#mapping-rows");
  els.mappingEmpty = document.querySelector("#mapping-empty");
  els.groupList = document.querySelector("#group-list");
  els.groupInput = document.querySelector("#group-input");
  els.groupPriorityInput = document.querySelector("#group-priority-input");
  els.lastUpdated = document.querySelector("#last-updated");
  els.modal = document.querySelector("#modal");
  els.modalTitle = document.querySelector("#modal-title");
  els.modalBody = document.querySelector("#modal-body");
  els.usageSummary = document.querySelector("#usage-summary");
  els.usageRows = document.querySelector("#usage-rows");
  els.usageEmpty = document.querySelector("#usage-empty");
  els.allowLanToggle = document.querySelector("#allow-lan-toggle");
  els.lanBlock = document.querySelector("#lan-block");
  els.lanUrls = document.querySelector("#lan-urls");
  els.localeSelect = document.querySelector("#locale-select");
  els.codexProviderSelect = document.querySelector("#codex-provider-select");
  els.codexKeepInput = document.querySelector("#codex-keep-input");
  els.codexRestorePinned = document.querySelector("#codex-restore-pinned");
  els.codexState = document.querySelector("#codex-state");
  els.codexOverview = document.querySelector("#codex-overview");
  els.codexBackupRows = document.querySelector("#codex-backup-rows");
  els.codexBackupEmpty = document.querySelector("#codex-backup-empty");
  els.codexToolOverlay = document.querySelector("#codex-tool-overlay");
  els.codexToolClose = document.querySelector("#codex-tool-close");
  els.terminalOverlay = document.querySelector("#terminal-overlay");
  els.terminalTitle = document.querySelector("#terminal-title");
  els.terminalOutput = document.querySelector("#terminal-output");
  els.terminalClose = document.querySelector("#terminal-close");
  els.aboutCodexVersion = document.querySelector("#about-codex-version");

  els.localeSelect.value = getLocale();
  applyTranslations();
  void safeInvoke("set_locale", { locale: getLocale() });
  els.localeSelect.addEventListener("change", () => {
    const draft = modalDraft();
    setLocale(els.localeSelect.value);
    void safeInvoke("set_locale", { locale: getLocale() });
    renderLocaleChange(draft);
  });

  els.routerToggle.addEventListener("change", async () => {
    const shouldRun = els.routerToggle.checked;
    routerSwitchBusy = true;
    els.routerToggle.disabled = true;
    els.processState.textContent = t(shouldRun ? "router.starting" : "router.stopping");
    const result = shouldRun ? await startRouter() : await stopRouter();
    routerSwitchBusy = false;
    if (result) {
      renderProcess(result);
    } else {
      await refresh();
    }
  });
  document.querySelector("#btn-check").addEventListener("click", checkAll);
  document.querySelector("#btn-add-channel").addEventListener("click", () => openChannelForm(-1));
  document.querySelector("#btn-add-mapping").addEventListener("click", () => openMappingForm(-1));
  document.querySelector("#btn-add-token").addEventListener("click", openTokenForm);
  document.querySelector("#btn-add-group").addEventListener("click", addGroup);
  document.querySelector("#tool-codex-sync").addEventListener("click", openCodexTool);
  document.querySelector("#about-github").addEventListener("click", () => {
    void openExternal("https://github.com/minkicc/mkrouter");
  });
  document.querySelector("#btn-codex-sync").addEventListener("click", runCodexSync);
  document.querySelector("#btn-codex-status").addEventListener("click", loadCodex);
  document.querySelector("#btn-codex-prune").addEventListener("click", runCodexPrune);
  els.codexToolClose.addEventListener("click", closeCodexTool);
  els.terminalClose.addEventListener("click", closeTerminal);
  document.querySelector("#modal-close").addEventListener("click", closeModal);
  document.querySelector("#modal-cancel").addEventListener("click", closeModal);
  document.querySelector("#modal-save").addEventListener("click", saveModal);

  els.noAuthToggle.addEventListener("change", async () => {
    config.no_auth = els.noAuthToggle.checked;
    els.tokenModule.hidden = config.no_auth;
    await saveConfig();
  });

  els.allowLanToggle.addEventListener("change", async () => {
    config.allow_lan = els.allowLanToggle.checked;
    if (!(await saveConfig())) {
      els.allowLanToggle.checked = !config.allow_lan;
      return;
    }
    toast(t("router.restartingForNetwork"));
    await safeInvoke("stop_router");
    await safeInvoke("start_router");
    setTimeout(refresh, 900);
  });

  document.querySelectorAll(".tab").forEach((tab) => {
    tab.addEventListener("click", () => switchTab(tab.dataset.tab));
  });

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && usageTabActive()) {
      startUsageUpdates();
    } else {
      stopUsageUpdates();
    }
  });

  document.querySelectorAll("[data-copy]").forEach((btn) => {
    btn.addEventListener("click", () => {
      copyText(btn.dataset.copy === "base-url" ? els.baseUrl.textContent : "");
    });
  });

  document.addEventListener("click", (e) => {
    if (!e.target.closest(".row-action-menu")) {
      document.querySelectorAll(".action-menu:popover-open").forEach((menu) => menu.hidePopover());
    }
  });

  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action]");
    if (btn) handleAction(btn.dataset.action, btn);
  });

  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-copy-url]");
    if (btn) copyText(btn.dataset.copyUrl);
  });

  document.addEventListener("change", (e) => {
    if (e.target.matches('#modal-body [name="auth_method"]')) {
      syncChannelAuthPanels();
      return;
    }
    if (e.target.matches('#modal-body [name="api_key_placement"]')) {
      syncAPIKeyFields();
      return;
    }
    const input = e.target.closest('[data-action="group-priority"]');
    if (input) updateGroupPriority(input);
  });

  refresh();
  setInterval(refresh, 3000);
});
