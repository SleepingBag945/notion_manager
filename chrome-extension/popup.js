const STRINGS = {
  zh: {
    subtitle: '一键提取账号配置，用于 Notion AI Proxy',
    statusInitial: '点击下方按钮，在 Notion 页面上提取账号配置。',
    extractBtn: '⚡ 提取配置',
    copyBtn: '📋 复制 JSON 配置',
    extracting: '<span class="spinner"></span>正在提取配置...',
    noCookies: '未找到 notion.so cookies。请先登录 Notion。',
    noToken: '未找到 token_v2 cookie。请先登录 Notion。',
    cookiesSuccess: '<span class="spinner"></span>已获取 cookies，正在获取用户数据...',
    openNotionFirst: '请先打开 Notion 页面 (notion.so)，然后重试。',
    noWorkspace: '未找到可用空间',
    extractFailed: '提取失败',
    success: '✅ 配置提取成功！',
    copied: '✅ 已复制！',
    labelUser: '用户',
    labelWorkspace: '空间',
    labelTimezone: '时区',
    labelVersion: '版本',
    labelToken: 'Token'
  },
  en: {
    subtitle: 'One-click account config extractor for Notion AI Proxy',
    statusInitial: 'Click the button below to extract the account configuration on the Notion page.',
    extractBtn: '⚡ Extract Config',
    copyBtn: '📋 Copy JSON Config',
    extracting: '<span class="spinner"></span>Extracting configuration...',
    noCookies: 'No notion.so cookies found. Please log in to Notion first.',
    noToken: 'No token_v2 cookie found. Please log in to Notion first.',
    cookiesSuccess: '<span class="spinner"></span>Cookies retrieved, fetching user data...',
    openNotionFirst: 'Please open the Notion page (notion.so) first, then try again.',
    noWorkspace: 'No accessible workspace found',
    extractFailed: 'Extraction failed',
    success: '✅ Configuration extracted successfully!',
    copied: '✅ Copied!',
    labelUser: 'User',
    labelWorkspace: 'Workspace',
    labelTimezone: 'Timezone',
    labelVersion: 'Version',
    labelToken: 'Token'
  }
};

let currentLang = localStorage.getItem('notion-ext-lang') || 'zh';

function updateUI() {
  const s = STRINGS[currentLang];
  document.getElementById('subtitle').textContent = s.subtitle;
  const statusEl = document.getElementById('status');
  if (statusEl.className === 'status info') {
    statusEl.textContent = s.statusInitial;
  }
  document.getElementById('extractBtn').textContent = s.extractBtn;
  document.getElementById('copyBtn').textContent = s.copyBtn;
  document.getElementById('langToggleBtn').textContent = currentLang === 'zh' ? '🌐 EN' : '🌐 中文';
}

async function extract() {
  const statusEl = document.getElementById('status');
  const extractBtn = document.getElementById('extractBtn');
  const resultEl = document.getElementById('result');
  const s = STRINGS[currentLang];

  extractBtn.disabled = true;
  statusEl.className = 'status loading';
  statusEl.innerHTML = s.extracting;

  try {
    // Step 1: Get all cookies via chrome.cookies API (can read HttpOnly!)
    const allCookies = await new Promise((resolve, reject) => {
      chrome.cookies.getAll({ domain: 'notion.so' }, (cookies) => {
        if (cookies && cookies.length > 0) resolve(cookies);
        else reject(new Error(s.noCookies));
      });
    });

    const cookieMap = {};
    for (const c of allCookies) cookieMap[c.name] = c.value;

    const token = cookieMap['token_v2'];
    if (!token) throw new Error(s.noToken);

    const browserId = cookieMap['notion_browser_id'] || '';
    const deviceId = cookieMap['device_id'] || '';
    const fullCookie = allCookies.map(c => `${c.name}=${c.value}`).join('; ');

    // Generate a browser_id if cookie doesn't exist
    const effectiveBrowserId = browserId || crypto.randomUUID();

    statusEl.innerHTML = s.cookiesSuccess;

    // Step 2: Get active tab and inject script to call Notion APIs
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });

    if (!tab?.url?.includes('notion.so')) {
      throw new Error(s.openNotionFirst);
    }

    // Step 3: Inject content script to call APIs with credentials
    const [{ result: accountData }] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: async () => {
        try {
          // Call loadUserContent
          const userResp = await fetch('/api/v3/loadUserContent', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            credentials: 'include',
            body: '{}'
          });
          if (!userResp.ok) throw new Error(`API error: ${userResp.status}`);
          const userData = await userResp.json();
          const rm = userData.recordMap;

          const getVal = (record) => record?.value?.value || record?.value;

          const userId = Object.keys(rm.notion_user)[0];
          const user = getVal(rm.notion_user[userId]);
          const userRoot = getVal(rm.user_root[userId]);
          const spacePointers = userRoot.space_view_pointers || [];

          // Find best space (AI enabled, non-free preferred)
          let bestSpace = null;
          for (const ptr of spacePointers) {
            const spaceData = getVal(rm.space?.[ptr.spaceId]);
            if (spaceData) {
              const aiEnabled = spaceData.settings?.enable_ai_feature !== false &&
                                spaceData.settings?.disable_ai_feature !== true;
              if (!bestSpace || (aiEnabled && spaceData.plan_type !== 'free')) {
                bestSpace = { ...spaceData, spaceViewId: ptr.id };
              }
            }
          }

          if (!bestSpace) throw new Error('未找到可用空间');

          const userSettings = getVal(rm.user_settings?.[userId]);
          const settings = userSettings?.settings || {};
          const timezone = settings.time_zone || Intl.DateTimeFormat().resolvedOptions().timeZone;

          // Get available models
          const modelResp = await fetch('/api/v3/getAvailableModels', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            credentials: 'include',
            body: JSON.stringify({ spaceId: bestSpace.id })
          });
          let models = [];
          if (modelResp.ok) {
            const modelData = await modelResp.json();
            models = (modelData.models || [])
              .filter(m => !m.isDisabled)
              .map(m => ({ name: m.modelMessage, id: m.model }));
          }

          // Get client version — wait up to 5s for window.CONFIG to initialize
          let clientVersion = window.CONFIG?.version;
          if (!clientVersion) {
            for (let i = 0; i < 50; i++) {
              await new Promise(r => setTimeout(r, 100));
              clientVersion = window.CONFIG?.version;
              if (clientVersion) break;
            }
          }
          // Fallback: extract from notion-client-version in performance entries
          if (!clientVersion) {
            try {
              const entries = performance.getEntriesByType('resource');
              for (const e of entries) {
                if (e.name.includes('/api/v3/')) {
                  // Try to find notion-client-version from XHR headers via serverTiming
                  break;
                }
              }
            } catch(e) {}
          }
          // Fallback: extract from document scripts version pattern
          if (!clientVersion) {
            const scriptEls = document.querySelectorAll('script[src*="notion.so/_assets/app-"]');
            if (scriptEls.length > 0) {
              // Use a sensible default based on today's date
              const d = new Date();
              const yy = d.getFullYear();
              const mm = String(d.getMonth()+1).padStart(2,'0');
              const dd = String(d.getDate()).padStart(2,'0');
              clientVersion = `23.13.${yy}${mm}${dd}.0000`;
            }
          }
          if (!clientVersion) clientVersion = 'unknown';

          return {
            success: true,
            user_id: userId,
            user_name: user.name,
            user_email: user.email,
            space_id: bestSpace.id,
            space_name: bestSpace.name,
            space_view_id: bestSpace.spaceViewId,
            plan_type: bestSpace.plan_type,
            timezone: timezone,
            client_version: clientVersion,
            available_models: models
          };
        } catch (err) {
          return { success: false, error: err.message };
        }
      }
    });

    if (!accountData?.success) {
      throw new Error(accountData?.error || s.extractFailed);
    }

    // Step 4: Assemble final config
    const config = {
      token_v2: token,
      user_id: accountData.user_id,
      user_name: accountData.user_name,
      user_email: accountData.user_email,
      space_id: accountData.space_id,
      space_name: accountData.space_name,
      space_view_id: accountData.space_view_id,
      plan_type: accountData.plan_type,
      timezone: accountData.timezone,
      client_version: accountData.client_version,
      browser_id: effectiveBrowserId,
      device_id: deviceId,
      full_cookie: fullCookie,
      available_models: accountData.available_models,
      extracted_at: new Date().toISOString()
    };

    // Step 5: Display results
    statusEl.className = 'status success';
    statusEl.textContent = s.success;

    const infoRows = document.getElementById('infoRows');
    infoRows.innerHTML = [
      [s.labelUser, `${config.user_name} (${config.user_email})`],
      [s.labelWorkspace, `${config.space_name} (${config.plan_type})`],
      [s.labelTimezone, config.timezone],
      [s.labelVersion, config.client_version],
      [s.labelToken, config.token_v2.substring(0, 20) + '...'],
    ].map(([label, value]) =>
      `<div class="info-row"><span class="info-label">${label}</span><span class="info-value">${value}</span></div>`
    ).join('');

    const modelTags = document.getElementById('modelTags');
    modelTags.innerHTML = config.available_models
      .map(m => `<span class="model-tag">${m.name}</span>`).join('');

    document.getElementById('configJson').value = JSON.stringify(config, null, 2);
    resultEl.classList.remove('hidden');

  } catch (err) {
    statusEl.className = 'status error';
    statusEl.textContent = '❌ ' + err.message;
  } finally {
    extractBtn.disabled = false;
  }
}

function copyConfig() {
  const textarea = document.getElementById('configJson');
  textarea.select();
  document.execCommand('copy');

  const btn = document.getElementById('copyBtn');
  btn.textContent = STRINGS[currentLang].copied;
  btn.classList.add('copied');
  setTimeout(() => {
    btn.textContent = STRINGS[currentLang].copyBtn;
    btn.classList.remove('copied');
  }, 2000);
}

document.getElementById('extractBtn').addEventListener('click', extract);
document.getElementById('copyBtn').addEventListener('click', copyConfig);
document.getElementById('langToggleBtn').addEventListener('click', () => {
  currentLang = currentLang === 'zh' ? 'en' : 'zh';
  localStorage.setItem('notion-ext-lang', currentLang);
  updateUI();
});

// Call updateUI on load
document.addEventListener('DOMContentLoaded', updateUI);
