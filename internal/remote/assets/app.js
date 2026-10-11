// Tether remote browser: watch a real Chrome tab live and drive it.
// Protocol: see internal/remote/ws.go. No build step, no dependencies.
'use strict';

(() => {
  const $ = (id) => document.getElementById(id);
  const el = {
    body: document.body,
    canvas: $('screen'),
    sink: $('sink'),
    stage: $('stage'),
    overlay: $('overlay'),
    overlayTitle: $('overlay-title'),
    overlayText: $('overlay-text'),
    address: $('address'),
    addressForm: $('address-form'),
    busy: $('busy'),
    back: $('back'),
    forward: $('forward'),
    reload: $('reload'),
    front: $('front'),
    control: $('control'),
    keyboard: $('keyboard'),
    fullscreen: $('fullscreen'),
    status: $('status'),
    fps: $('fps'),
    toggleTabs: $('toggle-tabs'),
    tabList: $('tab-list'),
    newTab: $('new-tab'),
    foot: $('foot'),
    toast: $('toast'),
    logout: $('logout'),
    dialog: $('dialog'),
    dialogForm: $('dialog-form'),
    dialogTitle: $('dialog-title'),
    dialogMessage: $('dialog-message'),
    dialogInput: $('dialog-input'),
    dialogCancel: $('dialog-cancel'),
  };
  const ctx2d = el.canvas.getContext('2d', { alpha: false });
  const narrow = window.matchMedia('(max-width: 760px)');

  const state = {
    tabs: [],
    current: null, // tab id being watched
    ws: null,
    meta: null, // FrameMeta of the frame on screen
    control: true,
    decoding: false,
    pending: null, // newest undecoded frame
    frames: 0,
    retry: 0,
    retryTimer: null,
    ended: false,
  };

  // ---------------------------------------------------------------- utils

  let toastTimer;
  function toast(msg, isErr) {
    el.toast.textContent = msg;
    el.toast.classList.toggle('err', !!isErr);
    el.toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.toast.hidden = true; }, isErr ? 6000 : 3000);
  }

  function overlay(title, text) {
    if (title === null) {
      el.overlay.hidden = true;
      return;
    }
    el.overlayTitle.textContent = title;
    el.overlayText.textContent = text || '';
    el.overlay.hidden = false;
  }

  function setStatus(kind, title) {
    el.status.className = 'status ' + kind;
    el.status.title = title;
    if (kind !== 'live') el.fps.textContent = '';
  }

  async function api(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
      credentials: 'same-origin',
    });
    if (res.status === 401) {
      location.href = 'login';
      throw new Error('signed out');
    }
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).message || msg; } catch (_) { /* not JSON */ }
      throw new Error(msg);
    }
    return res.status === 204 ? null : res.json();
  }

  function setControls(enabled) {
    for (const b of [el.back, el.forward, el.reload, el.front]) b.disabled = !enabled;
  }

  // ----------------------------------------------------------------- tabs

  function host(u) {
    try { return new URL(u).host || u; } catch (_) { return u; }
  }

  function renderTabs() {
    el.tabList.textContent = '';
    if (!state.tabs.length) {
      const li = document.createElement('li');
      li.className = 'empty';
      li.textContent = 'No tabs open.';
      el.tabList.append(li);
    }
    for (const t of state.tabs) {
      const li = document.createElement('li');
      const b = document.createElement('button');
      b.className = 'tab' + (t.id === state.current ? ' active' : '');
      b.type = 'button';
      b.title = t.url;
      const title = document.createElement('span');
      title.className = 't';
      title.textContent = t.title || host(t.url) || 'New tab';
      const url = document.createElement('span');
      url.className = 'u';
      url.textContent = t.url;
      b.append(title, url);
      if (t.session) {
        const badge = document.createElement('span');
        badge.className = 'badge' + (t.owned ? ' owned' : '');
        badge.textContent = t.owned ? 'Tether' : 'In use';
        badge.title = t.owned
          ? 'Opened by Tether (closed when idle)'
          : 'Your tab, currently attached to Tether';
        b.append(badge);
      }
      b.addEventListener('click', () => {
        connect(t.id);
        if (narrow.matches) toggleTabs(false);
      });
      const x = document.createElement('button');
      x.className = 'close';
      x.type = 'button';
      x.title = 'Close tab';
      x.setAttribute('aria-label', 'Close ' + (t.title || t.url));
      x.textContent = '×';
      x.addEventListener('click', (e) => {
        e.stopPropagation();
        closeTab(t);
      });
      li.append(b, x);
      el.tabList.append(li);
    }
  }

  async function refreshTabs() {
    try {
      const data = await api('GET', 'api/tabs');
      state.tabs = data.tabs || [];
      renderTabs();
    } catch (e) {
      if (e.message !== 'signed out') setStatus('down', 'Cannot reach Tether: ' + e.message);
    }
  }

  async function newTab(url) {
    try {
      const t = await api('POST', 'api/tabs', url ? { url } : {});
      await refreshTabs();
      connect(t.id);
      if (!url) {
        el.address.value = '';
        el.address.focus();
      }
    } catch (e) {
      toast('Could not open a tab: ' + e.message, true);
    }
  }

  async function closeTab(t) {
    let force = false;
    if (!t.owned) {
      if (!window.confirm('This is one of your own Chrome tabs. Close it in Chrome?')) return;
      force = true;
    }
    try {
      await api('DELETE', 'api/tabs/' + encodeURIComponent(t.id) + (force ? '?force=true' : ''));
    } catch (e) {
      toast('Could not close the tab: ' + e.message, true);
    }
    if (t.id === state.current) disconnect('Tab closed', 'Pick another tab, or open a new one.');
    refreshTabs();
  }

  function toggleTabs(open) {
    if (narrow.matches) {
      el.body.classList.toggle('tabs-open', open);
    } else {
      el.body.classList.toggle('tabs-closed', !open);
    }
    el.toggleTabs.setAttribute('aria-expanded', String(open));
  }

  function tabsOpen() {
    return narrow.matches ? el.body.classList.contains('tabs-open') : !el.body.classList.contains('tabs-closed');
  }

  // ------------------------------------------------------------ websocket

  function send(msg) {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) state.ws.send(JSON.stringify(msg));
  }

  function disconnect(title, text) {
    clearTimeout(state.retryTimer);
    if (state.ws) {
      state.ws.onclose = null;
      state.ws.close();
      state.ws = null;
    }
    state.current = null;
    state.meta = null;
    el.canvas.hidden = true;
    el.dialog.hidden = true;
    setControls(false);
    setStatus('', 'Disconnected');
    el.busy.hidden = true;
    history.replaceState(null, '', location.pathname);
    document.title = 'Tether';
    overlay(title, text);
    renderTabs();
  }

  function connect(id) {
    if (state.ws && state.current === id && state.ws.readyState <= WebSocket.OPEN) return;
    clearTimeout(state.retryTimer);
    if (state.ws) {
      state.ws.onclose = null;
      state.ws.close();
    }
    if (state.current !== id) {
      state.retry = 0;
      state.meta = null;
    }
    state.current = id;
    state.ended = false;
    history.replaceState(null, '', '#tab=' + encodeURIComponent(id));
    renderTabs();
    setStatus('connecting', 'Connecting…');
    if (!state.meta) overlay('Connecting…', 'Attaching to the tab.');

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const base = location.pathname.replace(/[^/]*$/, '');
    const ws = new WebSocket(proto + '//' + location.host + base + 'ws/' + encodeURIComponent(id));
    ws.binaryType = 'arraybuffer';
    state.ws = ws;

    ws.onopen = () => {
      state.retry = 0;
      setControls(true);
      setStatus('live', 'Live');
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') onText(JSON.parse(ev.data));
      else onFrame(ev.data);
    };
    ws.onclose = (ev) => {
      if (state.ws !== ws) return;
      state.ws = null;
      setControls(false);
      if (state.ended) return;
      if (ev.code === 1008 || ev.code === 1003) {
        disconnect('Cannot open this tab', ev.reason || 'The server refused the connection.');
        return;
      }
      // Reconnect with backoff: networks drop, laptops sleep.
      const delay = Math.min(10000, 500 * 2 ** state.retry++);
      setStatus('down', 'Disconnected; retrying');
      state.retryTimer = setTimeout(() => {
        refreshTabs().then(() => {
          if (state.tabs.some((t) => t.id === id)) connect(id);
          else disconnect('Tab closed', 'That tab is gone. Pick another, or open a new one.');
        });
      }, delay);
    };
  }

  function onText(m) {
    switch (m.t) {
      case 'info':
        if (document.activeElement !== el.address) el.address.value = m.url || '';
        document.title = (m.title || host(m.url) || 'Tab') + ' · Tether';
        {
          const t = state.tabs.find((x) => x.id === state.current);
          if (t && (t.url !== m.url || t.title !== m.title)) {
            t.url = m.url;
            t.title = m.title;
            renderTabs();
          }
        }
        break;
      case 'busy':
        el.busy.hidden = !m.busy;
        break;
      case 'error':
        toast(m.message || m.code, true);
        break;
      case 'clipboard':
        if (m.text && navigator.clipboard) {
          navigator.clipboard.writeText(m.text).then(
            () => toast('Copied from the tab'),
            () => toast('Copy blocked by this browser', true),
          );
        }
        break;
      case 'dialog':
        showDialog(m.dialog || {});
        break;
      case 'ended':
        state.ended = true;
        disconnect('Tab closed', m.code === 'browser_disconnected'
          ? 'Tether lost its connection to Chrome.'
          : 'That tab was closed. Pick another, or open a new one.');
        refreshTabs();
        break;
      default:
        break;
    }
  }

  // -------------------------------------------------------------- dialogs

  // alert/confirm/prompt are drawn by Chrome, outside the page, so they
  // never show up in the video: Tether relays them and the answer.
  function showDialog(d) {
    if (d.closed) {
      el.dialog.hidden = true;
      return;
    }
    el.dialogTitle.textContent = (host(d.url) || 'This page') + ' says';
    el.dialogMessage.textContent = d.type === 'beforeunload' ? 'Leave this page? Changes you made may not be saved.' : d.message || '';
    el.dialogInput.hidden = d.type !== 'prompt';
    el.dialogInput.value = d.defaultPrompt || '';
    el.dialogCancel.hidden = d.type === 'alert';
    el.dialog.hidden = false;
    (d.type === 'prompt' ? el.dialogInput : $('dialog-ok')).focus();
  }

  function answerDialog(accept) {
    send({ t: 'dialog', accept, text: accept && !el.dialogInput.hidden ? el.dialogInput.value : '' });
    el.dialog.hidden = true;
    focusSink();
  }

  el.dialogForm.addEventListener('submit', (e) => {
    e.preventDefault();
    answerDialog(true);
  });
  el.dialogCancel.addEventListener('click', () => answerDialog(false));
  el.dialog.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') answerDialog(false);
  });

  // ---------------------------------------------------------------- video

  function onFrame(buf) {
    state.pending = buf;
    state.frames++;
    if (!state.decoding) drawNext();
  }

  async function drawNext() {
    const buf = state.pending;
    if (!buf) return;
    state.pending = null;
    state.decoding = true;
    try {
      const view = new DataView(buf);
      const n = view.getUint32(0);
      const meta = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 4, n)));
      const blob = new Blob([new Uint8Array(buf, 4 + n)], { type: 'image/jpeg' });
      const bmp = await createImageBitmap(blob);
      if (el.canvas.width !== bmp.width || el.canvas.height !== bmp.height) {
        el.canvas.width = bmp.width;
        el.canvas.height = bmp.height;
        sizeCanvas();
      }
      ctx2d.drawImage(bmp, 0, 0);
      bmp.close();
      state.meta = meta;
      el.canvas.hidden = false;
      if (!el.overlay.hidden && state.current) overlay(null);
    } catch (e) {
      // A corrupt frame is not worth interrupting the stream for.
    } finally {
      state.decoding = false;
      if (state.pending) drawNext();
    }
  }

  // Fit the frame inside the stage, keeping its aspect ratio.
  function sizeCanvas() {
    const w = el.canvas.width;
    const h = el.canvas.height;
    if (!w || !h) return;
    const r = el.stage.getBoundingClientRect();
    const scale = Math.min(r.width / w, r.height / h);
    el.canvas.style.width = Math.floor(w * scale) + 'px';
    el.canvas.style.height = Math.floor(h * scale) + 'px';
  }
  new ResizeObserver(sizeCanvas).observe(el.stage);

  setInterval(() => {
    // Chrome only sends frames when the page repaints, so a still page
    // reads "idle", not "0 fps".
    if (state.ws && state.ws.readyState === WebSocket.OPEN) el.fps.textContent = state.frames ? state.frames + ' fps' : 'idle';
    state.frames = 0;
  }, 1000);

  // ---------------------------------------------------------------- input

  function modifiers(e) {
    return (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);
  }

  // Maps a pointer position to CSS pixels in the tab's viewport.
  function point(e) {
    const r = el.canvas.getBoundingClientRect();
    const m = state.meta;
    const vw = m && m.deviceWidth ? m.deviceWidth : el.canvas.width;
    const vh = m && m.deviceHeight ? m.deviceHeight : el.canvas.height;
    const x = Math.max(0, Math.min(r.width, e.clientX - r.left));
    const y = Math.max(0, Math.min(r.height, e.clientY - r.top));
    return { x: (x / r.width) * vw, y: (y / r.height) * vh };
  }

  const BUTTONS = ['left', 'middle', 'right', 'back', 'forward'];
  function pressedButton(buttons) {
    if (buttons & 1) return 'left';
    if (buttons & 2) return 'right';
    if (buttons & 4) return 'middle';
    return 'none';
  }

  function canDrive() {
    return state.control && state.ws && state.ws.readyState === WebSocket.OPEN && state.meta;
  }

  function focusSink() {
    el.sink.focus({ preventScroll: true });
  }

  // Click counting, since pointer events carry no click count.
  let lastDown = { t: 0, x: 0, y: 0, button: '', count: 0 };

  el.canvas.addEventListener('pointerdown', (e) => {
    if (!canDrive()) return;
    e.preventDefault();
    focusSink();
    el.canvas.setPointerCapture(e.pointerId);
    const p = point(e);
    const button = BUTTONS[e.button] || 'left';
    const now = performance.now();
    const near = Math.abs(p.x - lastDown.x) < 5 && Math.abs(p.y - lastDown.y) < 5;
    const count = now - lastDown.t < 500 && near && button === lastDown.button ? Math.min(3, lastDown.count + 1) : 1;
    lastDown = { t: now, x: p.x, y: p.y, button, count };
    send({ t: 'mouse', type: 'mousePressed', x: p.x, y: p.y, button, buttons: e.buttons, clickCount: count, modifiers: modifiers(e) });
  });

  el.canvas.addEventListener('pointerup', (e) => {
    if (!canDrive()) return;
    e.preventDefault();
    const p = point(e);
    send({ t: 'mouse', type: 'mouseReleased', x: p.x, y: p.y, button: BUTTONS[e.button] || 'left', buttons: e.buttons, clickCount: lastDown.count, modifiers: modifiers(e) });
  });

  // Moves are coalesced to one per animation frame.
  let moveQueued = null;
  el.canvas.addEventListener('pointermove', (e) => {
    if (!canDrive()) return;
    const queued = moveQueued;
    moveQueued = e;
    if (queued) return;
    requestAnimationFrame(() => {
      const ev = moveQueued;
      moveQueued = null;
      if (!ev || !canDrive()) return;
      const p = point(ev);
      send({ t: 'mouse', type: 'mouseMoved', x: p.x, y: p.y, button: pressedButton(ev.buttons), buttons: ev.buttons, modifiers: modifiers(ev) });
    });
  });

  el.canvas.addEventListener('wheel', (e) => {
    if (!canDrive()) return;
    e.preventDefault();
    const p = point(e);
    const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? (state.meta.deviceHeight || 600) : 1;
    send({ t: 'wheel', x: p.x, y: p.y, deltaX: e.deltaX * unit, deltaY: e.deltaY * unit, modifiers: modifiers(e) });
  }, { passive: false });

  el.canvas.addEventListener('contextmenu', (e) => e.preventDefault());

  el.sink.addEventListener('focus', () => el.stage.classList.add('focused'));
  el.sink.addEventListener('blur', () => el.stage.classList.remove('focused'));

  const EDIT_COMMANDS = { a: 'selectAll', c: 'copy', x: 'cut', z: 'undo', y: 'redo' };

  function keyMessage(type, e, text) {
    const msg = { t: 'key', type, key: e.key, code: e.code, keyCode: e.keyCode, modifiers: modifiers(e) };
    if (text) msg.text = text;
    return msg;
  }

  el.sink.addEventListener('keydown', (e) => {
    if (!canDrive()) return;
    // Let the IME and mobile keyboards compose; their text arrives via
    // compositionend / input below.
    if (e.isComposing || e.key === 'Process' || e.key === 'Unidentified' || e.keyCode === 229) return;
    const shortcut = e.ctrlKey || e.metaKey;
    const k = e.key.length === 1 ? e.key.toLowerCase() : '';
    if (shortcut && k === 'v') return; // the paste event sends the local clipboard
    e.preventDefault();
    let text = '';
    if (e.key === 'Enter') text = '\r';
    else if (e.key.length === 1 && !shortcut) text = e.key;
    const msg = keyMessage('keyDown', e, text);
    if (shortcut && EDIT_COMMANDS[k]) {
      msg.commands = [k === 'z' && e.shiftKey ? 'redo' : EDIT_COMMANDS[k]];
    }
    send(msg);
    if (shortcut && k === 'c') send({ t: 'copy' });
  });

  el.sink.addEventListener('keyup', (e) => {
    if (!canDrive() || e.isComposing || e.keyCode === 229) return;
    e.preventDefault();
    send(keyMessage('keyUp', e));
  });

  el.sink.addEventListener('compositionend', (e) => {
    if (canDrive() && e.data) send({ t: 'text', text: e.data });
    el.sink.value = '';
  });

  // Mobile keyboards mostly skip keydown and only report input.
  el.sink.addEventListener('input', (e) => {
    if (e.isComposing) return;
    if (canDrive()) {
      if (e.inputType === 'insertText' && e.data) send({ t: 'text', text: e.data });
      else if (e.inputType === 'insertLineBreak') {
        const enter = { key: 'Enter', code: 'Enter', keyCode: 13 };
        send({ t: 'key', type: 'keyDown', ...enter, text: '\r' });
        send({ t: 'key', type: 'keyUp', ...enter });
      } else if (e.inputType === 'deleteContentBackward') {
        const bs = { key: 'Backspace', code: 'Backspace', keyCode: 8 };
        send({ t: 'key', type: 'keyDown', ...bs });
        send({ t: 'key', type: 'keyUp', ...bs });
      }
    }
    el.sink.value = '';
  });

  el.sink.addEventListener('paste', (e) => {
    if (!canDrive()) return;
    e.preventDefault();
    const text = e.clipboardData && e.clipboardData.getData('text/plain');
    if (text) send({ t: 'text', text });
  });

  // -------------------------------------------------------------- toolbar

  el.addressForm.addEventListener('submit', (e) => {
    e.preventDefault();
    const url = el.address.value.trim();
    if (!url) return;
    el.address.blur();
    if (state.current && state.ws && state.ws.readyState === WebSocket.OPEN) {
      send({ t: 'nav', url });
      focusSink();
    } else {
      newTab(url);
    }
  });
  el.address.addEventListener('focus', () => el.address.select());
  el.back.addEventListener('click', () => send({ t: 'back' }));
  el.forward.addEventListener('click', () => send({ t: 'forward' }));
  el.reload.addEventListener('click', () => send({ t: 'reload' }));
  el.front.addEventListener('click', () => {
    send({ t: 'front' });
    toast('Brought to the front in Chrome');
  });
  el.control.addEventListener('click', () => {
    state.control = !state.control;
    el.control.classList.toggle('on', state.control);
    el.control.setAttribute('aria-pressed', String(state.control));
    el.control.title = state.control
      ? 'You are in control: input goes to the tab. Click to watch only.'
      : 'Watching only: input is not sent. Click to take control.';
    el.canvas.classList.toggle('watch', !state.control);
    toast(state.control ? 'You are in control' : 'Watching only');
  });
  el.keyboard.addEventListener('click', focusSink);
  el.fullscreen.addEventListener('click', () => {
    if (document.fullscreenElement) document.exitFullscreen();
    else el.stage.requestFullscreen().catch(() => toast('Full screen is not available here', true));
  });
  el.toggleTabs.addEventListener('click', () => toggleTabs(!tabsOpen()));
  el.stage.addEventListener('pointerdown', () => { if (narrow.matches && tabsOpen()) toggleTabs(false); }, true);
  el.newTab.addEventListener('click', () => newTab(''));

  // ----------------------------------------------------------------- boot

  async function boot() {
    toggleTabs(!narrow.matches);
    try {
      const info = await api('GET', 'api/info');
      el.foot.textContent = 'Tether ' + info.version + (info.browser ? ' · ' + info.browser : '');
      el.logout.hidden = !info.auth;
    } catch (_) { /* refreshTabs reports */ }
    await refreshTabs();
    const m = /tab=([^&]+)/.exec(location.hash);
    const want = m && decodeURIComponent(m[1]);
    if (want && state.tabs.some((t) => t.id === want)) connect(want);
    else if (narrow.matches) toggleTabs(true);
    setInterval(() => { if (!document.hidden) refreshTabs(); }, 3000);
  }

  boot();
})();
