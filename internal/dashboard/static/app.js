"use strict";

const state = { payload: null, filter: "", timer: null };
const byId = (id) => document.getElementById(id);

function node(tag, className, text) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (text !== undefined) element.textContent = String(text);
  return element;
}

function shortSHA(value) {
  return value ? value.slice(0, 8) : "—";
}

function formatTime(value) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit",
  }).format(date);
}

function relativeTime(value) {
  if (!value) return "—";
  const seconds = Math.round((new Date(value).getTime() - Date.now()) / 1000);
  const absolute = Math.abs(seconds);
  const formatter = new Intl.RelativeTimeFormat("zh-CN", { numeric: "auto" });
  if (absolute < 60) return formatter.format(seconds, "second");
  if (absolute < 3600) return formatter.format(Math.round(seconds / 60), "minute");
  if (absolute < 86400) return formatter.format(Math.round(seconds / 3600), "hour");
  return formatter.format(Math.round(seconds / 86400), "day");
}

function statusClass(status) {
  const value = (status || "").toLowerCase();
  if (["success", "delivered", "dispatched"].includes(value)) return "good";
  if (["error", "permanent_error", "failed"].includes(value)) return "bad";
  return "warn";
}

function badge(status) {
  return node("span", `badge ${statusClass(status)}`, status || "unknown");
}

function matches(...values) {
  if (!state.filter) return true;
  const haystack = values.filter(Boolean).join(" ").toLowerCase();
  return haystack.includes(state.filter);
}

function githubURL(repo, pull) {
  return `https://github.com/${repo}/pull/${pull}`;
}

function multicaIssueURL(workspaceURL, issueKey) {
  if (!workspaceURL) return "";
  return `${workspaceURL.replace(/\/+$/, "")}/issues/${encodeURIComponent(issueKey)}`;
}

function setText(id, value) {
  byId(id).textContent = String(value);
}

function setEmptyState(id, filteredCount, totalCount, emptyText, noMatchText) {
  const target = byId(id);
  target.hidden = filteredCount !== 0;
  if (filteredCount === 0) {
    target.textContent = state.filter && totalCount > 0 ? noMatchText : emptyText;
  }
}

function renderHealth(payload) {
  const health = byId("health");
  health.className = "health";
  const label = health.querySelector("span:last-child");
  const latest = payload.data.poll_runs[0];
  if (!latest) {
    health.classList.add("warn");
    label.textContent = "等待 poll heartbeat";
    return;
  }
  if (latest.status === "error") {
    health.classList.add("bad");
    label.textContent = "最近一次轮询失败";
    return;
  }
  const age = Date.now() - new Date(latest.completed_at || latest.started_at).getTime();
  if (age > payload.poll_interval_seconds * 2500) {
    health.classList.add("warn");
    label.textContent = "轮询已停滞";
    return;
  }
  health.classList.add("good");
  label.textContent = "轮询正常";
}

function renderMetrics(summary) {
  setText("metric-repos", summary.repositories);
  setText("metric-rounds", summary.rounds);
  setText("metric-feedback", summary.feedback);
  setText("metric-pending", summary.pending_deliveries);
  setText("metric-failures", summary.failures);
}

function renderRounds(rounds, workspaceURL) {
  const target = byId("rounds");
  target.replaceChildren();
  const filtered = rounds.filter((item) => matches(
    item.repo,
    item.pull_number,
    item.issue_key,
    item.head_sha,
    ...item.reviewer_ids,
    ...(item.reviewers || []).flatMap((reviewer) => [reviewer.id, reviewer.name]),
  ));
  setText("round-count", filtered.length);
  setEmptyState("rounds-empty", filtered.length, rounds.length, "还没有 review round。", "没有匹配的 review round。");
  for (const item of filtered) {
    const row = node("tr");
    const prCell = node("td");
    const link = node("a", "primary external", `${item.repo} #${item.pull_number}`);
    link.href = githubURL(item.repo, item.pull_number);
    link.target = "_blank";
    link.rel = "noreferrer";
    prCell.append(link, node("span", "secondary", `head ${shortSHA(item.head_sha)}`));
    const issue = node("td");
    const issueURL = multicaIssueURL(workspaceURL, item.issue_key);
    if (issueURL) {
      const issueLink = node("a", "primary external", item.issue_key);
      issueLink.href = issueURL;
      issueLink.target = "_blank";
      issueLink.rel = "noreferrer";
      issue.append(issueLink);
    } else {
      issue.append(node("span", "primary", item.issue_key));
    }
    const range = node("td", "mono", `${shortSHA(item.base_sha)} → ${shortSHA(item.head_sha)}`);
    const identities = item.reviewers?.length
      ? item.reviewers
      : (item.reviewer_ids || []).map((id) => ({ id, name: "" }));
    const reviewers = node("td", "primary", identities.map((reviewer) => reviewer.name || "未知 Reviewer").join(", ") || "—");
    reviewers.title = identities.map((reviewer) => reviewer.name ? `${reviewer.name} (${reviewer.id})` : reviewer.id).join(", ");
    const status = node("td");
    status.append(badge(item.status));
    const created = node("td", "event-time", relativeTime(item.created_at));
    created.title = formatTime(item.created_at);
    row.append(prCell, issue, range, reviewers, status, created);
    target.append(row);
  }
}

function renderFeedback(items) {
  const target = byId("feedback");
  target.replaceChildren();
  const filtered = items.filter((item) => matches(
    item.kind,
    item.repo,
    item.pull_number,
    item.issue_key,
    item.author,
    item.path,
    item.body_summary,
    item.status,
    item.delivery_status,
  ));
  setText("feedback-count", filtered.length);
  setEmptyState("feedback-empty", filtered.length, items.length, "还没有 review comment 回流。", "没有匹配的 review feedback。");
  for (const item of filtered) {
    const wrapper = node("article", "event");
    const icon = node("div", "event-icon", item.kind === "review" ? "RV" : "CM");
    const content = node("div");
    const title = node("p", "event-title");
    const link = node("a", "external", `${item.repo} #${item.pull_number}`);
    link.href = item.url || githubURL(item.repo, item.pull_number);
    link.target = "_blank";
    link.rel = "noreferrer";
    title.append(link, document.createTextNode(` → ${item.issue_key}`));
    const location = item.path ? `${item.path}${item.line ? `:${item.line}` : ""}` : "review submission";
    const copy = node("p", "event-copy", `${item.author || "unknown"} · ${location} · ${item.body_summary || "No body"}`);
    const delivery = badge(item.delivery_status || item.status);
    content.append(title, copy, delivery);
    const time = node("time", "event-time", relativeTime(item.occurred_at));
    time.title = formatTime(item.occurred_at);
    wrapper.append(icon, content, time);
    target.append(wrapper);
  }
}

function renderPolls(items) {
  const target = byId("polls");
  target.replaceChildren();
  const filtered = items.filter((item) => matches(item.status, item.error));
  setText("poll-count", filtered.length);
  setEmptyState("polls-empty", filtered.length, items.length, "尚无 poll heartbeat。", "没有匹配的 poll heartbeat。");
  for (const item of filtered.slice(0, 8)) {
    const wrapper = node("article", "event");
    const content = node("div");
    const title = node("p", "event-title", `Poll #${item.id}`);
    const copy = node("p", "event-copy", item.error || `Completed ${formatTime(item.completed_at)}`);
    content.append(title, copy, badge(item.status));
    const time = node("time", "event-time", relativeTime(item.completed_at || item.started_at));
    time.title = formatTime(item.completed_at || item.started_at);
    wrapper.append(content, time);
    target.append(wrapper);
  }
}

function renderCursors(items) {
  const target = byId("cursors");
  target.replaceChildren();
  const filtered = items.filter((cursor) => matches(cursor.repo, cursor.stream));
  for (const item of filtered) {
    const wrapper = node("div", "cursor");
    wrapper.append(node("span", "mono", `${item.repo} · ${item.stream}`), node("span", "", formatTime(item.last_success_at)));
    target.append(wrapper);
  }
  if (!target.children.length) {
    const message = state.filter && items.length > 0 ? "没有匹配的 cursor" : "暂无 cursor";
    target.append(node("div", "empty", message));
  }
}

function renderOutbox(items) {
  const target = byId("outbox");
  target.replaceChildren();
  const filtered = items.filter((item) => matches(item.event_key, item.kind, item.issue_key, item.status, item.last_error));
  setText("outbox-count", filtered.length);
  setEmptyState("outbox-empty", filtered.length, items.length, "Outbox 为空。", "没有匹配的 outbox event。");
  for (const item of filtered) {
    const row = node("tr");
    const event = node("td");
    event.append(node("span", "primary", item.kind), node("span", "secondary mono", item.event_key));
    const status = node("td");
    status.append(badge(item.status));
    const lastError = node("td", item.last_error ? "last-error" : "muted", item.last_error || "—");
    lastError.title = item.last_error || "";
    const updated = node("td", "event-time", relativeTime(item.updated_at));
    updated.title = formatTime(item.updated_at);
    row.append(event, node("td", "primary", item.issue_key), status, node("td", "mono", item.attempts), lastError, updated);
    target.append(row);
  }
}

function render() {
  if (!state.payload) return;
  const payload = state.payload;
  renderHealth(payload);
  renderMetrics(payload.data.summary);
  renderRounds(payload.data.rounds, payload.workspace_url);
  renderFeedback(payload.data.feedback);
  renderPolls(payload.data.poll_runs);
  renderCursors(payload.data.cursors);
  renderOutbox(payload.data.outbox);
  setText("subtitle", `${payload.workspace_prefix} workspace · ${payload.workspace_id} · 每 ${payload.poll_interval_seconds}s 轮询`);
  setText("updated", `数据生成于 ${formatTime(payload.data.generated_at)} · 自动刷新 ${payload.refresh_interval_seconds}s`);
  setText("version", `multica-github-dispatcher ${payload.version}`);
}

async function load() {
  const button = byId("refresh");
  button.disabled = true;
  byId("error").hidden = true;
  try {
    const response = await fetch("/api/v1/dashboard", { cache: "no-store" });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    state.payload = await response.json();
    render();
    clearTimeout(state.timer);
    state.timer = setTimeout(load, Math.max(2, state.payload.refresh_interval_seconds) * 1000);
  } catch (error) {
    const target = byId("error");
    target.textContent = `无法读取 Dispatcher 状态：${error.message}`;
    target.hidden = false;
    byId("health").className = "health bad";
    byId("health").querySelector("span:last-child").textContent = "Dashboard API 不可用";
    clearTimeout(state.timer);
    state.timer = setTimeout(load, 10000);
  } finally {
    button.disabled = false;
  }
}

byId("refresh").addEventListener("click", load);
byId("filter").addEventListener("input", (event) => {
  state.filter = event.target.value.trim().toLowerCase();
  render();
});
load();
