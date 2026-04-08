package httpserver

import (
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
)

var publisherConsoleTemplate = template.Must(template.New("publisher-console").Parse(`
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Publisher Console</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f4efe4;
      --panel: #fffdf8;
      --text: #111;
      --muted: #6f675a;
      --line: #d9d0c0;
      --black: #111;
      --green: #1e5c31;
      --red: #8b1f1f;
      font-family: "Helvetica Neue", Helvetica, Arial, sans-serif;
    }
    * { box-sizing: border-box; }
    body { margin: 0; background: radial-gradient(circle at top, #fbf7ef 0%, var(--bg) 60%, #e8dfcf 100%); color: var(--text); }
    main { max-width: 1200px; margin: 0 auto; padding: 28px 20px 60px; }
    h1, h2, h3 { margin: 0 0 12px; font-weight: 500; }
    p { margin: 0 0 12px; line-height: 1.5; }
    code { font-size: 0.95em; }
    .hero { margin-bottom: 24px; }
    .hero h1 { font-size: clamp(34px, 4vw, 58px); line-height: 0.95; letter-spacing: -0.03em; max-width: 10ch; }
    .hero p { color: var(--muted); max-width: 54ch; }
    .grid { display: grid; gap: 18px; grid-template-columns: 1fr; align-items: start; }
    .panel { background: rgba(255,253,248,0.93); border: 1px solid var(--line); padding: 18px; backdrop-filter: blur(14px); box-shadow: 0 14px 40px rgba(17,17,17,0.05); }
    .stack { display: grid; gap: 12px; }
    .split { display: grid; gap: 12px; grid-template-columns: 1fr 1fr; }
    .actions { display: flex; flex-wrap: wrap; gap: 10px; }
    label { display: block; font-size: 11px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; color: var(--muted); margin-bottom: 6px; }
    input, textarea, button { width: 100%; font: inherit; }
    input, textarea { background: #fff; color: var(--text); border: 1px solid #bfb4a0; padding: 10px 12px; }
    textarea { min-height: 110px; resize: vertical; }
    button { border: 0; background: var(--black); color: #fff; padding: 11px 14px; cursor: pointer; }
    button.secondary { background: #ddd2bd; color: #111; }
    button.ghost { background: transparent; color: #111; border: 1px solid var(--line); }
    button:disabled { opacity: 0.55; cursor: not-allowed; }
    .meta { font-size: 13px; color: var(--muted); }
    .message { min-height: 24px; font-size: 14px; }
    .message.ok { color: var(--green); }
    .message.error { color: var(--red); }
    .hidden { display: none !important; }
    .proof { padding: 12px; border: 1px solid var(--line); background: rgba(255,255,255,0.7); }
    .status { padding: 12px; border: 1px solid var(--line); background: rgba(255,255,255,0.82); }
    .pill { display: inline-block; padding: 4px 8px; border: 1px solid var(--line); font-size: 12px; margin-right: 6px; color: var(--muted); }
    .item { border-top: 1px solid var(--line); padding-top: 18px; margin-top: 18px; }
    .preview { background: #000; color: #fff; padding: 30px; min-height: 220px; display: flex; align-items: center; justify-content: center; text-align: center; }
    .preview p { margin: 0; font-size: 24px; line-height: 1.5; max-width: 24ch; }
    .muted-box { padding: 14px; border: 1px dashed var(--line); color: var(--muted); background: rgba(255,255,255,0.55); }
    .step-title { display: flex; align-items: center; gap: 10px; }
    .step-badge { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; border: 1px solid var(--line); border-radius: 999px; font-size: 13px; color: var(--muted); }
    .step-panel.locked { opacity: 0.75; }
    .step-panel.active { border-color: #bfb4a0; box-shadow: 0 18px 48px rgba(17,17,17,0.08); }
    .summary-row { display: flex; flex-wrap: wrap; gap: 8px; }
    .section-block { padding-top: 10px; border-top: 1px solid var(--line); }
    .compact textarea { min-height: 84px; }
    .wizard-nav { display: grid; gap: 10px; grid-template-columns: repeat(4, minmax(0, 1fr)); margin-bottom: 18px; }
    .wizard-tab { width: 100%; text-align: left; background: rgba(255,255,255,0.72); color: #111; border: 1px solid var(--line); padding: 12px; }
    .wizard-tab strong { display: block; font-weight: 600; margin-bottom: 4px; }
    .wizard-tab span { display: block; color: var(--muted); font-size: 13px; line-height: 1.3; }
    .wizard-tab.active { background: #111; color: #fff; border-color: #111; }
    .wizard-tab.active span { color: rgba(255,255,255,0.78); }
    .wizard-tab:disabled { opacity: 0.45; cursor: not-allowed; }
    .wizard-footer { display: flex; justify-content: space-between; gap: 12px; padding-top: 8px; border-top: 1px solid var(--line); }
    @media (max-width: 980px) {
      .grid { grid-template-columns: 1fr; }
      .split { grid-template-columns: 1fr; }
      .wizard-nav { grid-template-columns: 1fr; }
      .hero h1 { max-width: none; }
    }
  </style>
</head>
<body>
  <main>
    <section class="hero">
      <h1>Publisher Console</h1>
      <p>This page is for a simple local test flow: create an account, link one proof, load or create a playlist, edit notes, then export and send to FF1.</p>
    </section>

    <section class="wizard-nav">
      <button id="tab1" class="wizard-tab" type="button">
        <strong>1. Sign In</strong>
        <span>Create or reuse a local account.</span>
      </button>
      <button id="tab2" class="wizard-tab" type="button">
        <strong>2. Verify Proof</strong>
        <span>Link your wallet. ENS is optional.</span>
      </button>
      <button id="tab3" class="wizard-tab" type="button">
        <strong>3. Playlist</strong>
        <span>Create or load a playlist.</span>
      </button>
      <button id="tab4" class="wizard-tab" type="button">
        <strong>4. Notes</strong>
        <span>Edit notes and export to FF1.</span>
      </button>
    </section>

    <div class="grid">
      <section id="step1" class="panel stack step-panel" data-step="1">
        <div class="step-title">
          <span class="step-badge">1</span>
          <div>
            <h2>Sign In</h2>
            <p class="meta">Use your own name for now. For local testing, the easiest path is <strong>Use Local Test Account</strong>.</p>
          </div>
        </div>
        <div id="signInMessage" class="message"></div>
        <div id="authStatus" class="status">
          <strong>Not signed in yet.</strong>
        </div>
        <div id="authCard" class="stack">
          <div>
            <label for="displayName">Display name</label>
            <input id="displayName" placeholder="Sean Moss-Pultz">
          </div>
          <div class="actions">
            <button id="registerButton" type="button">Create Account With Passkey</button>
            <button id="loginButton" type="button" class="secondary">Sign In With Passkey</button>
            <button id="localSessionButton" type="button" class="ghost">Use Local Test Account</button>
          </div>
        </div>
        <div id="accountCard" class="stack hidden">
          <div class="summary-row">
            <span class="pill" id="accountName">Publisher</span>
            <span class="pill">proofs: <span id="proofCount">0</span></span>
          </div>
          <p class="meta">DP identity: <code id="publisherKey"></code></p>
          <div class="actions">
            <button id="refreshButton" type="button" class="ghost">Refresh Account</button>
            <button id="logoutButton" type="button" class="secondary">Log Out</button>
          </div>
        </div>
        <div class="wizard-footer">
          <span class="meta">Start here. The simplest path is still <strong>Use Local Test Account</strong>.</span>
          <button id="nextFromSignIn" type="button" class="ghost">Next</button>
        </div>
      </section>

      <section id="proofPanel" class="panel stack step-panel locked" data-step="2">
        <div class="step-title">
          <span class="step-badge">2</span>
          <div>
            <h2>Verify One Proof</h2>
            <p class="meta">The fastest path is to verify your wallet address. ENS is optional.</p>
          </div>
        </div>
        <div id="proofMessage" class="message"></div>
        <div id="proofLocked" class="muted-box">Sign in first.</div>
        <div id="proofContent" class="stack hidden">
          <div>
            <h3>Linked proofs</h3>
            <div id="proofList" class="stack"></div>
          </div>
          <div class="stack">
            <div>
              <label for="walletAddress">ETH address</label>
              <input id="walletAddress" placeholder="0x...">
            </div>
            <div class="actions">
              <button id="connectWalletButton" type="button" class="ghost">Use Connected Wallet</button>
              <button id="linkWalletButton" type="button">Verify ETH Address</button>
            </div>
          </div>
          <div class="stack">
            <div>
              <label for="ensName">ENS name</label>
              <input id="ensName" placeholder="casey.eth">
            </div>
            <div class="actions">
              <button id="linkEnsButton" type="button" class="ghost">Verify ENS Name</button>
            </div>
          </div>
        </div>
        <div class="wizard-footer">
          <button id="backToSignIn" type="button" class="ghost">Back</button>
          <button id="nextFromProof" type="button" class="ghost">Next</button>
        </div>
      </section>

      <section id="playlistPanel" class="panel stack step-panel locked" data-step="3">
        <div class="step-title">
          <span class="step-badge">3</span>
          <div>
            <h2>Create Or Load A Playlist</h2>
            <p class="meta">Fastest path: create a new test playlist here. It will open automatically.</p>
          </div>
        </div>
        <div id="playlistSetupMessage" class="message"></div>
        <div id="playlistLocked" class="muted-box">Sign in and verify a wallet first.</div>
        <div id="playlistSetup" class="stack hidden">
          <div class="stack">
            <div>
              <label for="newPlaylistTitle">Playlist title</label>
              <input id="newPlaylistTitle" placeholder="Casey Intermission Test">
            </div>
            <div>
              <label for="newPlaylistSlug">Playlist slug</label>
              <input id="newPlaylistSlug" placeholder="casey-intermission-test">
            </div>
            <div>
              <label for="newPlaylistWorkUrl">Work URL</label>
              <input id="newPlaylistWorkUrl" placeholder="https://example.com/work">
            </div>
            <div>
              <label for="newPlaylistWorkTitle">Work title</label>
              <input id="newPlaylistWorkTitle" placeholder="Test Work">
            </div>
            <div class="actions">
              <button id="createPlaylistButton" type="button">Create Playlist And Open It</button>
            </div>
            <p class="meta" id="createdPlaylistMeta"></p>
          </div>

          <div class="muted-box">
            Already have a playlist? Paste its slug or URL here instead.
          </div>
          <div class="split">
            <div>
              <label for="playlistRef">Playlist URL, ID, or slug</label>
              <input id="playlistRef" placeholder="casey-intermission-test">
            </div>
            <div class="actions" style="align-items:end">
              <button id="loadPlaylistButton" type="button">Load Playlist</button>
            </div>
          </div>
        </div>
        <div class="wizard-footer">
          <button id="backToProof" type="button" class="ghost">Back</button>
          <button id="nextFromPlaylist" type="button" class="ghost">Next</button>
        </div>
      </section>

      <section id="notesPanel" class="panel stack step-panel locked" data-step="4">
        <div class="step-title">
          <span class="step-badge">4</span>
          <div>
            <h2>Edit Notes And Export</h2>
            <p class="meta">Once a playlist is open, edit the note, save it, then send the JSON to FF1.</p>
          </div>
        </div>
        <div id="notesMessage" class="message"></div>
        <div id="notesLocked" class="muted-box">Create or load a playlist first.</div>
        <div id="playlistEditor" class="hidden stack">
          <div class="split">
            <div>
              <h3 id="playlistTitle">Playlist</h3>
              <p class="meta"><span id="playlistSlug"></span> · DP-1 <span id="playlistVersion"></span></p>
            </div>
            <div class="actions" style="align-items:start; justify-content:flex-end;">
              <button id="downloadPlaylistButton" type="button" class="ghost">Download Playlist JSON</button>
              <button id="copyPlaylistJsonButton" type="button" class="ghost">Copy Playlist JSON</button>
              <button id="copyFf1CliCommandButton" type="button" class="ghost">Copy ff1 Command</button>
            </div>
          </div>

          <div class="preview">
            <p id="previewText">Load a playlist to preview a note.</p>
          </div>

          <div class="panel compact" style="padding:16px; margin:0;">
            <h3>Playlist Note</h3>
            <div class="stack">
              <div>
                <label for="playlistNoteText">Note text</label>
                <textarea id="playlistNoteText" maxlength="500"></textarea>
              </div>
              <div>
                <label for="playlistNoteDuration">Display duration</label>
                <input id="playlistNoteDuration" type="number" min="1" value="20">
              </div>
              <div class="actions">
                <button id="previewPlaylistNoteButton" type="button" class="ghost">Preview Playlist Note</button>
                <button id="savePlaylistNoteButton" type="button">Save Playlist Note</button>
                <button id="clearPlaylistNoteButton" type="button" class="secondary">Clear Playlist Note</button>
              </div>
            </div>
          </div>

          <div>
            <h3>Work Notes</h3>
            <div id="itemList" class="stack"></div>
          </div>
        </div>
        <div class="wizard-footer">
          <button id="backToPlaylist" type="button" class="ghost">Back</button>
          <span class="meta">When the note looks right, download the JSON and run the copied <code>ff1 send</code> command.</span>
        </div>
      </section>
    </div>
  </main>

  <template id="itemTemplate">
    <div class="item stack">
      <div>
        <h3 data-title></h3>
        <p class="meta" data-id></p>
      </div>
      <div>
        <label>Note text</label>
        <textarea maxlength="500" data-text></textarea>
      </div>
      <div>
        <label>Display duration</label>
        <input type="number" min="1" value="20" data-duration>
      </div>
      <div class="actions">
        <button type="button" class="ghost" data-preview>Preview Item Note</button>
        <button type="button" data-save>Save Item Note</button>
        <button type="button" class="secondary" data-clear>Clear Item Note</button>
      </div>
    </div>
  </template>

  <script>
    const state = {
      account: null,
      playlist: null,
      playlistRef: '',
      createdPlaylist: null,
    };

    const signInMessage = document.getElementById('signInMessage');
    const proofMessage = document.getElementById('proofMessage');
    const playlistSetupMessage = document.getElementById('playlistSetupMessage');
    const notesMessage = document.getElementById('notesMessage');
    const authCard = document.getElementById('authCard');
    const accountCard = document.getElementById('accountCard');
    const authStatus = document.getElementById('authStatus');
    const proofList = document.getElementById('proofList');
    const playlistEditor = document.getElementById('playlistEditor');
    const itemTemplate = document.getElementById('itemTemplate');
    const itemList = document.getElementById('itemList');
    const createdPlaylistMeta = document.getElementById('createdPlaylistMeta');
    const proofPanel = document.getElementById('proofPanel');
    const proofLocked = document.getElementById('proofLocked');
    const proofContent = document.getElementById('proofContent');
    const playlistPanel = document.getElementById('playlistPanel');
    const playlistLocked = document.getElementById('playlistLocked');
    const playlistSetup = document.getElementById('playlistSetup');
    const notesPanel = document.getElementById('notesPanel');
    const notesLocked = document.getElementById('notesLocked');
    const stepPanels = Array.from(document.querySelectorAll('[data-step]'));
    const wizardTabs = [1, 2, 3, 4].map((n) => document.getElementById('tab' + n));

    function setMessage(el, text, kind = '') {
      el.textContent = text || '';
      el.className = 'message' + (kind ? ' ' + kind : '');
    }

    function toBase64Url(bytes) {
      const str = btoa(String.fromCharCode(...new Uint8Array(bytes)));
      return str.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
    }

    function fromBase64Url(value) {
      if (!value) return new ArrayBuffer(0);
      const padded = value.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((value.length + 3) % 4);
      const binary = atob(padded);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
      return bytes.buffer;
    }

    function publicKeyCreationOptionsFromJSON(json) {
      const options = structuredClone(json.publicKey || json.response || json);
      options.challenge = fromBase64Url(options.challenge);
      options.user.id = fromBase64Url(options.user.id);
      if (Array.isArray(options.excludeCredentials)) {
        options.excludeCredentials = options.excludeCredentials.map((item) => ({ ...item, id: fromBase64Url(item.id) }));
      }
      return options;
    }

    function publicKeyRequestOptionsFromJSON(json) {
      const options = structuredClone(json.publicKey || json.response || json);
      options.challenge = fromBase64Url(options.challenge);
      if (Array.isArray(options.allowCredentials)) {
        options.allowCredentials = options.allowCredentials.map((item) => ({ ...item, id: fromBase64Url(item.id) }));
      }
      return options;
    }

    function credentialToJSON(credential) {
      if (!credential) return null;
      const response = credential.response || {};
      const out = {
        id: credential.id,
        rawId: toBase64Url(credential.rawId),
        type: credential.type,
        response: {},
      };
      if (response.clientDataJSON) out.response.clientDataJSON = toBase64Url(response.clientDataJSON);
      if (response.attestationObject) out.response.attestationObject = toBase64Url(response.attestationObject);
      if (response.authenticatorData) out.response.authenticatorData = toBase64Url(response.authenticatorData);
      if (response.signature) out.response.signature = toBase64Url(response.signature);
      if (response.userHandle) out.response.userHandle = toBase64Url(response.userHandle);
      if (response.transports && typeof response.getTransports === 'function') out.response.transports = response.getTransports();
      if (credential.clientExtensionResults) out.clientExtensionResults = credential.clientExtensionResults;
      if (typeof credential.getClientExtensionResults === 'function') out.clientExtensionResults = credential.getClientExtensionResults();
      if (credential.authenticatorAttachment) out.authenticatorAttachment = credential.authenticatorAttachment;
      return out;
    }

    async function api(path, options = {}) {
      const response = await fetch(path, {
        credentials: 'include',
        headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
        ...options,
      });
      const text = await response.text();
      const body = text ? JSON.parse(text) : null;
      if (!response.ok) {
        const message = body?.message || body?.error || response.statusText;
        throw new Error(message);
      }
      return body;
    }

    function ensurePasskeyAvailable() {
      const hasCredentials = !!(window.PublicKeyCredential && navigator.credentials);
      if (hasCredentials) return;
      if (!window.isSecureContext) {
        throw new Error('Passkeys need a secure context. For local testing, open this page on http://localhost:8787, not a .local hostname.');
      }
      throw new Error('This browser does not expose passkey APIs here.');
    }

    async function registerPasskey() {
      ensurePasskeyAvailable();
      const displayName = document.getElementById('displayName').value.trim();
      setMessage(signInMessage, '');
      const options = await api('/api/v1/publisher/register/options', { method: 'POST', body: JSON.stringify({ displayName }) });
      const credential = await navigator.credentials.create({ publicKey: publicKeyCreationOptionsFromJSON(options) });
      await api('/api/v1/publisher/register/verify', { method: 'POST', body: JSON.stringify({ credential: credentialToJSON(credential) }) });
      await refreshAccount('Account created.', 'ok');
    }

    async function loginPasskey() {
      ensurePasskeyAvailable();
      setMessage(signInMessage, '');
      const options = await api('/api/v1/publisher/login/options', { method: 'POST', body: JSON.stringify({}) });
      const credential = await navigator.credentials.get({ publicKey: publicKeyRequestOptionsFromJSON(options) });
      await api('/api/v1/publisher/login/verify', { method: 'POST', body: JSON.stringify({ credential: credentialToJSON(credential) }) });
      await refreshAccount('Signed in.', 'ok');
    }

    async function beginLocalSession() {
      const displayName = document.getElementById('displayName').value.trim();
      setMessage(signInMessage, '');
      await api('/api/v1/publisher/local-session', { method: 'POST', body: JSON.stringify({ displayName }) });
      await refreshAccount('Local test account ready.', 'ok');
    }

    async function logoutPublisher() {
      await api('/api/v1/publisher/logout', { method: 'POST', body: JSON.stringify({}) });
      state.account = null;
      renderAccount();
      setMessage(signInMessage, 'Signed out.', 'ok');
    }

    async function refreshAccount(message = '', kind = '') {
      try {
        state.account = await api('/api/v1/publisher/me');
        renderAccount();
        setMessage(signInMessage, message, kind);
      } catch (err) {
        state.account = null;
        renderAccount();
        if (message || kind) {
          setMessage(signInMessage, err.message || 'Not signed in.', 'error');
        } else {
          setMessage(signInMessage, 'Not signed in yet. Create an account or sign in above.', '');
        }
      }
    }

    function renderAccount() {
      const signedIn = !!state.account;
      const proofs = Array.isArray(state.account?.proofs) ? state.account.proofs : [];
      authCard.classList.toggle('hidden', signedIn);
      accountCard.classList.toggle('hidden', !signedIn);
      if (!signedIn) {
        authStatus.innerHTML = '<strong>Not signed in yet.</strong><p class="meta" style="margin-top:8px;">Use your own name for now. Create a passkey account or sign in, then verify your wallet. For local passkey testing, use <code>http://localhost:8787/publisher</code>.</p>';
        proofList.innerHTML = '';
        document.getElementById('playlistRef').placeholder = 'Sign in first, then load a playlist slug or URL';
        renderFlow();
        return;
      }
      authStatus.innerHTML = '<strong>Signed in as ' + state.account.displayName + '.</strong><p class="meta" style="margin-top:8px;">Next: verify your wallet if you have not already, then use <strong>Create Test Playlist</strong> below.</p>';
      document.getElementById('playlistRef').placeholder = 'Load an existing playlist slug/URL, or create one above first';
      document.getElementById('accountName').textContent = state.account.displayName;
      document.getElementById('publisherKey').textContent = state.account.publisherKey;
      document.getElementById('proofCount').textContent = String(proofs.length);
      proofList.innerHTML = '';
      if (proofs.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'proof meta';
        empty.textContent = 'No verified proofs yet. Next step: click "Use Connected Wallet" or enter an address, then click "Verify ETH Address".';
        proofList.appendChild(empty);
      } else {
        proofs.forEach((proof) => {
          const row = document.createElement('div');
          row.className = 'proof';
          row.innerHTML = '<span class="pill">' + proof.type + '</span><code>' + proof.value + '</code>';
          proofList.appendChild(row);
        });
      }
      renderFlow();
    }

    function renderFlow() {
      const signedIn = !!state.account;
      const hasProof = signedIn && Array.isArray(state.account?.proofs) && state.account.proofs.length > 0;
      const hasPlaylist = !!state.playlist;

      proofPanel.classList.toggle('locked', !signedIn);
      proofLocked.classList.toggle('hidden', signedIn);
      proofContent.classList.toggle('hidden', !signedIn);

      playlistPanel.classList.toggle('locked', !hasProof);
      playlistLocked.classList.toggle('hidden', hasProof);
      playlistSetup.classList.toggle('hidden', !hasProof);

      notesPanel.classList.toggle('locked', !hasPlaylist);
      notesLocked.classList.toggle('hidden', hasPlaylist);
      playlistEditor.classList.toggle('hidden', !hasPlaylist);
      renderWizard();
    }

    function furthestUnlockedStep() {
      const signedIn = !!state.account;
      const hasProof = signedIn && Array.isArray(state.account?.proofs) && state.account.proofs.length > 0;
      const hasPlaylist = !!state.playlist;
      if (hasPlaylist) return 4;
      if (hasProof) return 3;
      if (signedIn) return 2;
      return 1;
    }

    function setActiveStep(step) {
      const maxStep = furthestUnlockedStep();
      state.activeStep = Math.max(1, Math.min(step, maxStep));
      renderWizard();
    }

    function renderWizard() {
      const maxStep = furthestUnlockedStep();
      if (!state.activeStep || state.activeStep > maxStep) {
        state.activeStep = maxStep;
      }
      stepPanels.forEach((panel) => {
        const step = Number(panel.getAttribute('data-step'));
        const active = step === state.activeStep;
        panel.classList.toggle('hidden', !active);
        panel.classList.toggle('active', active);
      });
      wizardTabs.forEach((tab, index) => {
        const step = index + 1;
        const active = step === state.activeStep;
        tab.classList.toggle('active', active);
        tab.disabled = step > maxStep;
      });
      document.getElementById('nextFromSignIn').disabled = maxStep < 2;
      document.getElementById('nextFromProof').disabled = maxStep < 3;
      document.getElementById('nextFromPlaylist').disabled = maxStep < 4;
    }

    async function linkWalletProof() {
      if (!state.account) throw new Error('Sign in first.');
      let address = document.getElementById('walletAddress').value.trim();
      if (!address && window.ethereum) {
        const [selected] = await window.ethereum.request({ method: 'eth_requestAccounts' });
        address = selected || '';
        document.getElementById('walletAddress').value = address;
      }
      if (!address) throw new Error('Wallet address is required.');
      const challenge = await api('/api/v1/publisher/proofs/wallet/challenge', { method: 'POST', body: JSON.stringify({ address }) });
      if (!window.ethereum) throw new Error('No injected Ethereum wallet found.');
      const signature = await window.ethereum.request({ method: 'personal_sign', params: [challenge.message, address] });
      await api('/api/v1/publisher/proofs/wallet/verify', { method: 'POST', body: JSON.stringify({ signature }) });
      await refreshAccount('Wallet proof linked.', 'ok');
      setMessage(proofMessage, 'Wallet proof linked.', 'ok');
      setActiveStep(3);
    }

    async function linkENSProof() {
      if (!state.account) throw new Error('Sign in first.');
      const name = document.getElementById('ensName').value.trim();
      if (!name) throw new Error('ENS name is required.');
      await api('/api/v1/publisher/proofs/ens/verify', {
        method: 'POST',
        body: JSON.stringify({ name }),
      });
      await refreshAccount('ENS proof linked.', 'ok');
      setMessage(proofMessage, 'ENS proof linked.', 'ok');
    }

    async function useConnectedWallet() {
      if (!window.ethereum) throw new Error('No injected Ethereum wallet found.');
      const [selected] = await window.ethereum.request({ method: 'eth_requestAccounts' });
      document.getElementById('walletAddress').value = selected || '';
    }

    function currentPlaylistRequestBody() {
      const cloned = structuredClone(state.playlist);
      cloned.note = buildNote(document.getElementById('playlistNoteText').value, Number(document.getElementById('playlistNoteDuration').value));
      cloned.items = cloned.items.map((item) => {
        const text = document.querySelector('[data-item-text="' + item.id + '"]').value;
        const duration = Number(document.querySelector('[data-item-duration="' + item.id + '"]').value);
        return { ...item, note: buildNote(text, duration) };
      });
      return cloned;
    }

    function buildNote(text, duration) {
      const trimmed = (text || '').trim();
      if (!trimmed) return null;
      return { text: trimmed, display_duration: duration > 0 ? duration : 20 };
    }

    function normalizePlaylistRef(ref) {
      const trimmed = (ref || '').trim();
      if (!trimmed) return '';
      if (trimmed.includes('/api/v1/playlists/')) {
        return trimmed.split('/api/v1/playlists/').pop().split(/[?#]/)[0];
      }
      return trimmed;
    }

    function buildNewPlaylistBody() {
      const title = document.getElementById('newPlaylistTitle').value.trim();
      const slug = document.getElementById('newPlaylistSlug').value.trim();
      const workUrl = document.getElementById('newPlaylistWorkUrl').value.trim();
      const workTitle = document.getElementById('newPlaylistWorkTitle').value.trim() || title || 'Test Work';
      if (!title) throw new Error('Playlist title is required.');
      if (!workUrl) throw new Error('Work URL is required.');
      return {
        dpVersion: '1.1.1',
        title,
        slug,
        items: [
          {
            title: workTitle,
            source: workUrl,
            duration: 20000,
            license: 'open',
          },
        ],
      };
    }

    async function createPlaylist() {
      if (!state.account) throw new Error('Sign in first.');
      const playlist = await api('/api/v1/playlists', {
        method: 'POST',
        body: JSON.stringify(buildNewPlaylistBody()),
      });
      state.createdPlaylist = playlist;
      const ref = playlist.url || playlist.slug || playlist.id || '';
      createdPlaylistMeta.textContent = ref ? ('Created: ' + ref) : 'Playlist created.';
      if (ref) {
        document.getElementById('playlistRef').value = ref;
      }
      await ensurePlaylistChannel(ref, playlist);
      state.playlistRef = normalizePlaylistRef(ref);
      state.playlist = playlist;
      renderPlaylist();
      setMessage(playlistSetupMessage, 'Playlist created and opened.', 'ok');
      setMessage(notesMessage, 'Playlist created and loaded. Edit a note below, then save it.', 'ok');
      setActiveStep(4);
      return playlist;
    }

    async function ensurePlaylistChannel(ref, playlist) {
      if (!state.account) return null;
      const normalizedRef = normalizePlaylistRef(ref);
      const titleBase = (playlist?.title || 'Playlist').trim();
      const slugBase = (playlist?.slug || normalizedRef || 'playlist').trim();
      const playlistURL = new URL('/api/v1/playlists/' + encodeURIComponent(normalizedRef), window.location.origin).toString();
      return api('/api/v1/channels', {
        method: 'POST',
        body: JSON.stringify({
          title: titleBase + ' Channel',
          slug: slugBase + '-channel',
          version: '1.0.0',
          playlists: [playlistURL],
          summary: 'Auto-created local test channel for publisher note editing.',
        }),
      });
    }

    async function loadPlaylist() {
      const rawRef = document.getElementById('playlistRef').value.trim();
      const ref = normalizePlaylistRef(rawRef);
      if (!ref) throw new Error('Playlist ref is required.');
      state.playlistRef = ref;
      state.playlist = await api('/api/v1/playlists/' + encodeURIComponent(ref), { headers: {} });
      renderPlaylist();
      setActiveStep(4);
    }

    function renderPlaylist() {
      if (!state.playlist) {
        renderFlow();
        return;
      }
      document.getElementById('playlistTitle').textContent = state.playlist.title;
      document.getElementById('playlistSlug').textContent = state.playlist.slug;
      document.getElementById('playlistVersion').textContent = state.playlist.dpVersion;
      document.getElementById('playlistNoteText').value = state.playlist.note?.text || '';
      document.getElementById('playlistNoteDuration').value = String(state.playlist.note?.display_duration || 20);
      document.getElementById('previewText').textContent = state.playlist.note?.text || 'Preview this note on the frame.';
      itemList.innerHTML = '';
      state.playlist.items.forEach((item, index) => {
        const node = itemTemplate.content.firstElementChild.cloneNode(true);
        node.querySelector('[data-title]').textContent = item.title || ('Item ' + (index + 1));
        node.querySelector('[data-id]').textContent = item.id;
        const textField = node.querySelector('[data-text]');
        const durationField = node.querySelector('[data-duration]');
        textField.value = item.note?.text || '';
        durationField.value = String(item.note?.display_duration || 20);
        textField.setAttribute('data-item-text', item.id);
        durationField.setAttribute('data-item-duration', item.id);
        node.querySelector('[data-preview]').addEventListener('click', () => {
          const note = buildNote(textField.value, Number(durationField.value));
          document.getElementById('previewText').textContent = note?.text || 'Preview this note on the frame.';
        });
        textField.addEventListener('focus', () => {
          const note = buildNote(textField.value, Number(durationField.value));
          if (note?.text) document.getElementById('previewText').textContent = note.text;
        });
        node.querySelector('[data-save]').addEventListener('click', () => savePlaylist('Item note saved.'));
        node.querySelector('[data-clear]').addEventListener('click', () => {
          textField.value = '';
          durationField.value = '20';
          savePlaylist('Item note cleared.');
        });
        itemList.appendChild(node);
      });
      renderFlow();
    }

    async function savePlaylist(message) {
      if (!state.account) throw new Error('Sign in first.');
      if (!state.playlist) throw new Error('Load a playlist first.');
      const body = currentPlaylistRequestBody();
      const ref = state.playlist.slug || state.playlist.id || state.playlistRef;
      state.playlist = await api('/api/v1/playlists/' + encodeURIComponent(ref), {
        method: 'PUT',
        body: JSON.stringify(body),
      });
      renderPlaylist();
      setMessage(notesMessage, message + ' Next step: download the playlist JSON or copy the ff1 command.', 'ok');
    }

    function exportPlaylistJSON() {
      if (!state.playlist) throw new Error('Load a playlist first.');
      return JSON.stringify(state.playlist, null, 2);
    }

    function downloadPlaylistJSON() {
      const payload = exportPlaylistJSON();
      const blob = new Blob([payload], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a');
      const base = state.playlist?.slug || state.playlist?.id || 'playlist';
      anchor.href = url;
      anchor.download = base + '.json';
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
      setMessage(notesMessage, 'Playlist JSON downloaded. Next step: send it to FF1 with ff1.', 'ok');
    }

    async function copyPlaylistJSON() {
      const payload = exportPlaylistJSON();
      await navigator.clipboard.writeText(payload);
      setMessage(notesMessage, 'Playlist JSON copied.', 'ok');
    }

    function currentPlaylistFilename() {
      const base = state.playlist?.slug || state.playlist?.id || 'playlist';
      return base + '.json';
    }

    async function copyFF1CLICommand() {
      if (!state.playlist) throw new Error('Load a playlist first.');
      const filename = currentPlaylistFilename();
      const command = 'ff1 send "' + filename + '" -d office';
      await navigator.clipboard.writeText(command);
      setMessage(notesMessage, 'ff1 command copied. Update the device name or file path if needed, then run it in the terminal.', 'ok');
    }

    document.getElementById('previewPlaylistNoteButton').addEventListener('click', async () => {
      const note = buildNote(document.getElementById('playlistNoteText').value, Number(document.getElementById('playlistNoteDuration').value));
      document.getElementById('previewText').textContent = note?.text || 'Preview this note on the frame.';
    });
    document.getElementById('playlistNoteText').addEventListener('focus', async () => {
      const note = buildNote(document.getElementById('playlistNoteText').value, Number(document.getElementById('playlistNoteDuration').value));
      if (note?.text) document.getElementById('previewText').textContent = note.text;
    });

    document.getElementById('registerButton').addEventListener('click', async () => {
      try { await registerPasskey(); } catch (err) { setMessage(signInMessage, err.message, 'error'); }
    });
    document.getElementById('loginButton').addEventListener('click', async () => {
      try { await loginPasskey(); } catch (err) { setMessage(signInMessage, err.message, 'error'); }
    });
    document.getElementById('localSessionButton').addEventListener('click', async () => {
      try { await beginLocalSession(); } catch (err) { setMessage(signInMessage, err.message, 'error'); }
    });
    document.getElementById('logoutButton').addEventListener('click', async () => {
      try { await logoutPublisher(); } catch (err) { setMessage(signInMessage, err.message, 'error'); }
    });
    document.getElementById('refreshButton').addEventListener('click', async () => {
      try { await refreshAccount('Account refreshed.', 'ok'); } catch (err) { setMessage(signInMessage, err.message, 'error'); }
    });
    document.getElementById('connectWalletButton').addEventListener('click', async () => {
      try { await useConnectedWallet(); setMessage(proofMessage, 'Wallet address loaded from browser wallet.', 'ok'); } catch (err) { setMessage(proofMessage, err.message, 'error'); }
    });
    document.getElementById('linkWalletButton').addEventListener('click', async () => {
      try { await linkWalletProof(); } catch (err) { setMessage(proofMessage, err.message, 'error'); }
    });
    document.getElementById('linkEnsButton').addEventListener('click', async () => {
      try { await linkENSProof(); } catch (err) { setMessage(proofMessage, err.message, 'error'); }
    });
    document.getElementById('createPlaylistButton').addEventListener('click', async () => {
      try { await createPlaylist(); } catch (err) { setMessage(playlistSetupMessage, err.message, 'error'); }
    });
    document.getElementById('loadPlaylistButton').addEventListener('click', async () => {
      try { await loadPlaylist(); setMessage(notesMessage, 'Playlist loaded. Next step: edit a note below and click save.', 'ok'); } catch (err) { setMessage(playlistSetupMessage, err.message, 'error'); }
    });
    document.getElementById('downloadPlaylistButton').addEventListener('click', async () => {
      try { downloadPlaylistJSON(); } catch (err) { setMessage(notesMessage, err.message, 'error'); }
    });
    document.getElementById('copyPlaylistJsonButton').addEventListener('click', async () => {
      try { await copyPlaylistJSON(); } catch (err) { setMessage(notesMessage, err.message, 'error'); }
    });
    document.getElementById('copyFf1CliCommandButton').addEventListener('click', async () => {
      try { await copyFF1CLICommand(); } catch (err) { setMessage(notesMessage, err.message, 'error'); }
    });
    document.getElementById('savePlaylistNoteButton').addEventListener('click', async () => {
      try { await savePlaylist('Playlist note saved.'); } catch (err) { setMessage(notesMessage, err.message, 'error'); }
    });
    document.getElementById('clearPlaylistNoteButton').addEventListener('click', async () => {
      document.getElementById('playlistNoteText').value = '';
      document.getElementById('playlistNoteDuration').value = '20';
      try { await savePlaylist('Playlist note cleared.'); } catch (err) { setMessage(notesMessage, err.message, 'error'); }
    });
    wizardTabs.forEach((tab, index) => {
      tab.addEventListener('click', () => setActiveStep(index + 1));
    });
    document.getElementById('nextFromSignIn').addEventListener('click', () => setActiveStep(2));
    document.getElementById('backToSignIn').addEventListener('click', () => setActiveStep(1));
    document.getElementById('nextFromProof').addEventListener('click', () => setActiveStep(3));
    document.getElementById('backToProof').addEventListener('click', () => setActiveStep(2));
    document.getElementById('nextFromPlaylist').addEventListener('click', () => setActiveStep(4));
    document.getElementById('backToPlaylist').addEventListener('click', () => setActiveStep(3));

    renderFlow();
    refreshAccount();
  </script>
</body>
</html>
`))

func (h *Handler) PublisherConsolePage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	_ = publisherConsoleTemplate.Execute(c.Writer, nil)
}
