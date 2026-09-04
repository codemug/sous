/* Sous board — progressive enhancement over a server-rendered board.
 *
 * The page works with no JavaScript: every model carries a "Deploy to…"
 * menu of real <form> posts, one per connected node. This script layers on
 * three things and nothing the no-JS path cannot already do:
 *   1. live refresh   — poll GET /api/nodes, redraw node bays in place
 *   2. drag to deploy — the same POST /api/deploy/{id}/{node} a menu item
 *                       fires, reachable by dragging a model onto a node
 *   3. fit preview    — while dragging, ghost the model's true length into
 *                       each node so "does it fit" is seen before the drop
 *
 * "Memory is a length": every width below is calc(var(--gib) * GiB), the
 * same scale the server rendered with, recomputed here for the real
 * viewport. The tailnet drops ~1 request in 10, so every fetch tolerates
 * failure and keeps the last good state rather than blanking the board. */
(function () {
  "use strict";
  var board = document.getElementById("board");
  if (!board) return;
  var bays = document.getElementById("bays");
  var POLL_MS = 4000;
  var fleet = [];        // last good GET /api/nodes
  var dragging = null;   // {recipe, gib}

  function setScale() {
    // px per GiB from the widest pool and the board's real width, so
    // asus-gx10 (121.6) spans most of the row and aorus (15.9) is a stub.
    var maxPool = 16;
    fleet.forEach(function (n) { if (n.pool_gib > maxPool) maxPool = n.pool_gib; });
    // also consider the widest model so the shelf bars use the same ruler
    document.querySelectorAll(".model[data-footprint-gib]").forEach(function (m) {
      var g = parseFloat(m.getAttribute("data-footprint-gib")) || 0;
      if (g > maxPool) maxPool = g;
    });
    var avail = board.clientWidth - 32;
    if (avail < 320) avail = 320;
    board.style.setProperty("--gib", (avail / maxPool) + "px");
    drawRuler(maxPool);
  }

  function drawRuler(maxPool) {
    var r = document.getElementById("ruler");
    if (!r) return;
    r.innerHTML = "";
    var step = maxPool > 80 ? 20 : maxPool > 32 ? 10 : 4;
    for (var g = 0; g <= maxPool; g += step) {
      var t = document.createElement("div");
      t.className = "tick";
      t.style.left = "calc(var(--gib) * " + g + ")";
      var s = document.createElement("span");
      s.textContent = g + (g === 0 ? " GiB" : "");
      t.appendChild(s);
      r.appendChild(t);
    }
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function fmt(g) { return (Math.round(g * 10) / 10).toFixed(1); }

  // committed / margin exactly as the server and planOnNode compute them
  function committed(n) {
    var c = 0; (n.deployments || []).forEach(function (d) { c += d.weights_gib + d.kv_gib; }); return c;
  }
  function marginOf(n) { return n.pool_gib - n.reserve_gib - committed(n); }

  function stateOf(d) {
    // node path has no readiness probe: honour Docker's raw word, never a
    // green "ready" we cannot verify.
    switch (d.docker_status) {
      case "running": return { cls: "unknown", label: "running (docker)" };
      case "restarting": return { cls: "transitional", label: "restarting" };
      case "created": case "paused": return { cls: "transitional", label: d.docker_status };
      case "exited": case "dead": return { cls: "fault", label: d.docker_status };
      default: return { cls: "unknown", label: d.docker_status || "unknown" };
    }
  }

  function ageText(n) {
    if (!n.connected) return { cls: "gone", text: "disconnected — showing the last snapshot" };
    var a = Math.round(n.snapshot_age_s);
    if (a > 45) return { cls: "stale", text: "connected, but no snapshot for " + a + "s" };
    return { cls: "", text: "connected · snapshot " + a + "s old" };
  }

  function renderBays() {
    if (!bays) return;
    bays.innerHTML = "";
    fleet.forEach(function (n) {
      var bay = el("section", "bay");
      bay.setAttribute("data-node-id", n.node_id);
      if (!n.connected) bay.classList.add("offline");

      var head = el("div", "bay-head");
      head.appendChild(el("span", "id", n.node_id));
      var m = marginOf(n);
      var free = el("span", "free" + (m < 0 ? " over" : ""));
      free.textContent = fmt(m) + " GiB free of " + fmt(n.pool_gib);
      head.appendChild(free);
      var fr = ageText(n);
      var frDiv = el("div", "freshness " + fr.cls, fr.text);
      head.appendChild(frDiv);
      bay.appendChild(head);

      var bar = el("div", "bar");
      bar.style.width = "calc(var(--gib) * " + n.pool_gib + ")";
      (n.deployments || []).forEach(function (d) {
        var g = d.weights_gib + d.kv_gib;
        var st = stateOf(d);
        var w = el("div", "seg " + (st.cls === "fault" ? "fault" : st.cls === "transitional" ? "transitional" : "weights"));
        w.style.width = "calc(var(--gib) * " + g + ")";
        w.appendChild(el("span", "lab", d.recipe_id));
        w.title = d.recipe_id + " — " + st.label + (d.host_port ? " :" + d.host_port : "");
        bar.appendChild(w);
      });
      if (m > 0) {
        var free2 = el("div", "seg free");
        free2.style.width = "calc(var(--gib) * " + m + ")";
        bar.appendChild(free2);
      }
      var res = el("div", "seg reserve");
      res.style.width = "calc(var(--gib) * " + n.reserve_gib + ")";
      res.appendChild(el("span", "lab", "reserve"));
      bar.appendChild(res);
      bay.appendChild(bar);

      (n.deployments || []).forEach(function (d) {
        var st = stateOf(d);
        var row = el("div", "dep");
        row.appendChild(el("span", "dot " + st.cls));
        row.appendChild(el("span", "name", d.recipe_id));
        row.appendChild(el("span", "state " + st.cls, st.label + (d.host_port ? " · :" + d.host_port : "")));
        if (n.connected) {
          var stop = el("button", "btn-danger", "Stop " + d.recipe_id);
          stop.type = "button";
          stop.addEventListener("click", function () { undeploy(d.recipe_id, n.node_id, stop); });
          row.appendChild(stop);
        }
        bay.appendChild(row);
      });
      if (!(n.deployments || []).length) {
        bay.appendChild(el("p", "empty", n.connected ? "Nothing deployed here — drag a model in." : "No last-known deployments."));
      }

      if (n.connected) enableDrop(bay, n);
      bays.appendChild(bay);
    });
  }

  // ---- drag & drop --------------------------------------------------------
  function enableDrop(bay, node) {
    bay.addEventListener("dragover", function (ev) {
      if (!dragging) return;
      ev.preventDefault();
      var m = marginOf(node) - dragging.gib;
      bay.classList.toggle("drop-ok", m >= 0);
      bay.classList.toggle("drop-bad", m < 0);
      showGhost(bay, node, m);
    });
    bay.addEventListener("dragleave", function (ev) {
      if (!bay.contains(ev.relatedTarget)) clearGhost(bay);
    });
    bay.addEventListener("drop", function (ev) {
      ev.preventDefault();
      if (!dragging) return;
      var recipe = dragging.recipe;
      clearGhost(bay);
      deploy(recipe, node.node_id);
    });
  }
  function showGhost(bay, node, marginAfter) {
    clearGhost(bay);
    var bar = bay.querySelector(".bar");
    if (!bar) return;
    var ghost = el("div", "seg ghost" + (marginAfter < 0 ? " over" : ""));
    ghost.style.width = "calc(var(--gib) * " + dragging.gib + ")";
    ghost.appendChild(el("span", "lab", dragging.recipe + " " + fmt(dragging.gib)));
    // insert before the reserve segment so the overhang is visible past the wall
    var reserve = bar.querySelector(".seg.reserve");
    bar.insertBefore(ghost, reserve);
    bay._ghost = ghost;
  }
  function clearGhost(bay) {
    bay.classList.remove("drop-ok", "drop-bad");
    if (bay._ghost) { bay._ghost.remove(); bay._ghost = null; }
  }

  function wireDragSources() {
    document.querySelectorAll(".model[data-recipe-id]").forEach(function (m) {
      if (m.getAttribute("data-archived") === "true") return; // archived can't deploy
      m.setAttribute("draggable", "true");
      m.addEventListener("dragstart", function (ev) {
        dragging = {
          recipe: m.getAttribute("data-recipe-id"),
          gib: parseFloat(m.getAttribute("data-footprint-gib")) || 0,
        };
        ev.dataTransfer.setData("text/plain", dragging.recipe);
        ev.dataTransfer.effectAllowed = "copy";
      });
      m.addEventListener("dragend", function () {
        dragging = null;
        document.querySelectorAll(".bay").forEach(clearGhost);
      });
    });
  }

  // ---- actions ------------------------------------------------------------
  function deploy(recipe, node) {
    flash("Deploying " + recipe + " to " + node + "…");
    fetch("/api/deploy/" + encodeURIComponent(recipe) + "/" + encodeURIComponent(node), {
      method: "POST", headers: { "Accept": "application/json" },
    }).then(readResult).then(function (res) {
      if (res.ok) { flash("Deployed " + recipe + " to " + node + ". It will take a few minutes to load."); poll(); }
      else if (res.status === 409) { flash(recipe + " does not fit on " + node + (res.body && res.body.must_free ? " — free: " + res.body.must_free.join(", ") : "") + ".", true); }
      else { flash("Deploy failed: " + (res.message || ("HTTP " + res.status)), true); }
    }).catch(function () { flash("Deploy request did not reach the server — check the node and try again.", true); });
  }
  function undeploy(recipe, node, btn) {
    if (btn) { btn.disabled = true; }
    flash("Stopping " + recipe + " on " + node + "…");
    fetch("/api/undeploy/" + encodeURIComponent(recipe) + "/" + encodeURIComponent(node), {
      method: "POST", headers: { "Accept": "application/json" },
    }).then(readResult).then(function (res) {
      if (res.ok) { flash("Stopped " + recipe + " on " + node + "."); poll(); }
      else { flash("Stop failed: " + (res.message || ("HTTP " + res.status)), true); if (btn) btn.disabled = false; }
    }).catch(function () { flash("Stop request did not reach the server.", true); if (btn) btn.disabled = false; });
  }
  function readResult(r) {
    return r.text().then(function (t) {
      var body = null; try { body = JSON.parse(t); } catch (e) {}
      return { ok: r.ok, status: r.status, body: body, message: body && (body.error || body.message) };
    });
  }

  // ---- shelf status (derived from fleet) ---------------------------------
  function updateShelf() {
    document.querySelectorAll(".model[data-recipe-id]").forEach(function (m) {
      var id = m.getAttribute("data-recipe-id");
      var status = m.querySelector(".status");
      if (!status || m.getAttribute("data-archived") === "true") return;
      var on = null, cached = [];
      fleet.forEach(function (n) {
        (n.deployments || []).forEach(function (d) { if (d.recipe_id === id) on = { node: n.node_id, d: d }; });
        var repo = m.getAttribute("data-model-repo");
        if (repo && (n.cached_weight_repos || []).indexOf(repo) !== -1) cached.push(n.node_id);
      });
      if (on) {
        var st = stateOf(on.d);
        status.innerHTML = "";
        var s = el("span", "on", "on " + on.node + " · " + st.label);
        status.appendChild(s);
      } else if (cached.length) {
        status.textContent = "weights on " + cached.join(", ");
      } else {
        status.textContent = "not on any node";
      }
    });
  }

  // ---- polling ------------------------------------------------------------
  function poll() {
    fetch("/api/nodes", { headers: { "Accept": "application/json" } })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (data) {
        fleet = Array.isArray(data) ? data : [];
        board.classList.remove("reconnecting");
        setScale();
        renderBays();
        updateShelf();
      })
      .catch(function () { board.classList.add("reconnecting"); /* keep last good board */ });
  }

  var flashEl = document.getElementById("flash");
  function flash(msg, isErr) {
    if (!flashEl) return;
    flashEl.textContent = msg;
    flashEl.className = "flash" + (isErr ? " err" : "");
    flashEl.hidden = false;
  }

  wireDragSources();
  setScale();
  poll();
  setInterval(poll, POLL_MS);
  window.addEventListener("resize", setScale);
})();
