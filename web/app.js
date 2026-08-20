(() => {
  const qs = (selector, root = document) => root.querySelector(selector);
  const qsa = (selector, root = document) => [...root.querySelectorAll(selector)];

  qsa('[data-dialog]').forEach(button => button.addEventListener('click', () => qs(`#${button.dataset.dialog}`)?.showModal()));
  qsa('.dialog-close').forEach(button => button.addEventListener('click', () => button.closest('dialog')?.close()));
  qsa('.dialog-cancel').forEach(button => button.addEventListener('click', () => button.closest('dialog')?.close()));
  qsa('dialog').forEach(dialog => dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); }));
  qsa('[data-confirm]').forEach(form => form.addEventListener('submit', event => { if (!confirm(form.dataset.confirm)) event.preventDefault(); }));
  qsa('.copy-btn').forEach(button => button.addEventListener('click', () => copyText(button.dataset.copy, button)));

  const loginForm = qs('#login-form');
  loginForm?.addEventListener('keydown', event => {
    if (event.key !== 'Enter' || event.isComposing) return;
    event.preventDefault();
    loginForm.requestSubmit(qs('button[type="submit"]', loginForm));
  });

  const workspace = qs('#file-workspace');
  if (!workspace) return;

  const config = {
    csrf: workspace.dataset.csrf,
    username: workspace.dataset.username,
    role: workspace.dataset.role,
    initialPath: workspace.dataset.initialPath || '',
    quota: Number(workspace.dataset.quota || 0),
  };
  const state = {
    currentDir: config.initialPath,
    items: [],
    selected: null,
    details: null,
    sort: 'name',
    sortDirection: 1,
    sourceMode: false,
    activeRow: -1,
    selectedPaths: new Set(),
    lastSelectedIndex: -1,
    visibleItems: [],
    commandIndex: 0,
    dragPaths: [],
    directoryLoadToken: 0,
    treeLoadToken: 0,
    rowActionAnchor: null,
  };

  const elements = {
    tree: qs('#file-tree'), treeRoot: qs('#tree-root'), list: qs('#file-list'), uploadPath: qs('#upload-path'),
    logicalPath: qs('#logical-path'), contextLabel: qs('#context-label'), folderTitle: qs('#folder-title'), folderSummary: qs('#folder-summary'),
    directoryView: qs('#directory-view'), previewView: qs('#preview-view'), previewStage: qs('#preview-stage'),
    previewTitle: qs('#preview-title'), previewMeta: qs('#preview-meta'), metadata: qs('#file-metadata'), selectionActions: qs('#selection-actions'),
    shareToggle: qs('#share-toggle'), copyShare: qs('#copy-share'), copyPath: qs('#copy-path'), download: qs('#download-file'),
    sourceToggle: qs('#source-toggle'), edit: qs('#edit-file'), rename: qs('#rename-file'), remove: qs('#delete-file'),
    move: qs('#move-file'), batchActions: qs('#batch-actions'), batchCount: qs('#batch-count'), selectAll: qs('#select-all'),
    search: qs('#file-search'), storageLabel: qs('#storage-label'), storageMeter: qs('#storage-meter'), toast: qs('#toast-region'),
    rowActionMenu: qs('#row-action-menu'), main: qs('.workspace-main'), uploadOverlay: qs('#main-upload-overlay'),
    uploadDestination: qs('#main-upload-destination'),
  };

  initDivider();
  initMobileSidebar();
  initUpload();
  initActions();
  initMenus();
  initKeyboard();
  initTree();
  initializeWorkspace();

  async function api(endpoint, options = {}) {
    const response = await fetch(endpoint, { credentials: 'same-origin', ...options });
    const type = response.headers.get('content-type') || '';
    const payload = type.includes('application/json') ? await response.json() : { error: await response.text() };
    if (!response.ok) throw new Error(payload.error || '操作失败');
    return payload;
  }

  async function post(endpoint, values) {
    const body = new URLSearchParams({ csrf: config.csrf, ...values });
    return api(endpoint, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body });
  }

  async function loadDirectory(dir, options = {}) {
    const loadToken = ++state.directoryLoadToken;
    state.currentDir = dir || '';
    state.selected = null;
    state.details = null;
    state.activeRow = -1;
    state.selectedPaths.clear();
    state.lastSelectedIndex = -1;
    elements.uploadPath.value = state.currentDir;
    elements.directoryView.hidden = false;
    elements.previewView.hidden = true;
    elements.selectionActions.hidden = true;
    updateBatchBar();
    elements.contextLabel.textContent = '当前位置';
    elements.logicalPath.textContent = logicalPath(state.currentDir);
    elements.folderTitle.textContent = state.currentDir ? state.currentDir.split('/').pop() : config.username;
    elements.folderSummary.textContent = '正在读取文件…';
    elements.list.innerHTML = '<div class="skeleton-row"></div><div class="skeleton-row"></div><div class="skeleton-row"></div>';
    if (options.history !== false) {
      const target = state.currentDir ? `/files?path=${encodeURIComponent(state.currentDir)}` : '/files';
      history.pushState({ path: state.currentDir }, '', target);
    }
    markTreeSelection(state.currentDir);
    try {
      const result = await api(`/api/list?path=${encodeURIComponent(state.currentDir)}`);
      if (loadToken !== state.directoryLoadToken) return;
      state.items = result.items || [];
      elements.logicalPath.textContent = result.logical_path;
      updateUsage(result.used, result.quota);
      renderList();
    } catch (error) {
      if (loadToken !== state.directoryLoadToken) return;
      elements.list.innerHTML = '';
      renderEmpty('无法读取这个目录', error.message);
      toast(error.message, true);
    }
  }

  function renderList() {
    closeRowActionMenu();
    const query = elements.search.value.trim().toLocaleLowerCase();
    const items = state.items.filter(item => item.name.toLocaleLowerCase().includes(query));
    items.sort((a, b) => {
      if (a.type !== b.type) return a.type === 'dir' ? -1 : 1;
      let result;
      if (state.sort === 'size') result = a.size - b.size;
      else if (state.sort === 'modified') result = a.modified_ts - b.modified_ts;
      else result = a.name.localeCompare(b.name, 'zh-CN', { numeric: true, sensitivity: 'base' });
      return result * state.sortDirection;
    });
    state.visibleItems = items;
    elements.list.replaceChildren();
    elements.folderSummary.textContent = query ? `${items.length} 个匹配项` : `${state.items.length} 个项目`;
    if (!items.length) {
      renderEmpty(query ? '没有匹配的文件' : '这个目录还是空的', query ? '换个关键词试试' : '从左侧上传文件，或在底部新建内容');
      return;
    }
    items.forEach((item, index) => {
      const row = document.createElement('div');
      row.className = 'file-row-v2';
      row.role = 'row';
      row.tabIndex = index === 0 ? 0 : -1;
      row.dataset.path = item.path;
      row.draggable = true;
      row.title = item.type === 'dir' ? '双击打开，或拖拽移动' : '拖拽到文件夹以移动';
      row.innerHTML = `<label class="selection-cell"><input type="checkbox" aria-label="选择 ${escapeHTML(item.name)}"></label><span class="file-cell-name"><i class="list-file-icon ${item.type === 'dir' ? 'folder' : 'file'}"></i><strong></strong></span><span>${escapeHTML(item.modified)}</span><span>${item.type === 'dir' ? '—' : escapeHTML(item.size_label)}</span><span class="share-state ${item.shared ? 'on' : ''}"><i class="share-dot"></i>${item.shared ? '开启' : '关闭'}</span><span class="row-action-cell"></span>`;
      qs('.file-cell-name strong', row).textContent = item.name;
      buildRowActions(item, row);
      const checkbox = qs('input[type="checkbox"]', row);
      checkbox.checked = state.selectedPaths.has(item.path);
      if (checkbox.checked) row.classList.add('multi-selected');
      checkbox.addEventListener('click', event => { event.stopPropagation(); toggleMulti(item, row, checkbox.checked, event, index); });
      row.addEventListener('click', () => {
        clearMultiSelection();
        setSelected(item, row);
        if (item.type === 'file') showPreview(item);
      });
      row.addEventListener('dblclick', () => item.type === 'dir' ? openDirectory(item.path) : showPreview(item));
      row.addEventListener('focus', () => { state.activeRow = index; });
      bindDragSource(row, item);
      if (item.type === 'dir') bindMoveDropTarget(row, item.path);
      elements.list.append(row);
    });
    updateBatchBar();
  }

  function buildRowActions(item, row) {
    const cell = qs('.row-action-cell', row);
    const copyShare = document.createElement('button');
    copyShare.className = 'row-action-button'; copyShare.type = 'button'; copyShare.textContent = '复制链接';
    copyShare.disabled = !item.shared || !item.share_url;
    copyShare.title = copyShare.disabled ? '请先开启分享' : '复制分享链接';
    copyShare.setAttribute('aria-label', `复制“${item.name}”的分享链接`);
    copyShare.addEventListener('click', event => { event.stopPropagation(); copyText(item.share_url, copyShare); });

    const copyPath = document.createElement('button');
    copyPath.className = 'row-action-button'; copyPath.type = 'button'; copyPath.textContent = '复制路径';
    copyPath.setAttribute('aria-label', `复制“${item.name}”的路径`);
    copyPath.addEventListener('click', event => { event.stopPropagation(); copyText(item.logical_path, copyPath); });

    const download = document.createElement('a');
    download.className = 'row-action-button'; download.textContent = '下载'; download.download = '';
    download.href = item.content_url || `/content?path=${encodeURIComponent(item.path)}`;
    download.setAttribute('aria-label', `下载“${item.name}”`);
    download.hidden = item.type === 'dir';
    download.addEventListener('click', event => event.stopPropagation());

    const more = document.createElement('button');
    more.className = 'row-action-button more'; more.type = 'button'; more.textContent = '•••';
    more.setAttribute('aria-label', `更多“${item.name}”操作`); more.setAttribute('aria-haspopup', 'menu'); more.setAttribute('aria-expanded', 'false');
    more.addEventListener('click', event => { event.stopPropagation(); openRowActionMenu(item, row, more); });

    cell.addEventListener('click', event => event.stopPropagation());
    cell.addEventListener('dblclick', event => event.stopPropagation());
    cell.append(copyShare, copyPath, download, more);
  }

  function openRowActionMenu(item, row, anchor) {
    if (!elements.rowActionMenu.hidden && state.rowActionAnchor === anchor) { closeRowActionMenu(); return; }
    closeRowActionMenu();
    clearMultiSelection();
    setSelected(item, row);
    state.rowActionAnchor = anchor;
    anchor.setAttribute('aria-expanded', 'true');
    qs('#row-edit-file').hidden = !item.editable;
    elements.rowActionMenu.hidden = false;
    const anchorRect = anchor.getBoundingClientRect();
    const menuRect = elements.rowActionMenu.getBoundingClientRect();
    const left = Math.max(8, Math.min(window.innerWidth - menuRect.width - 8, anchorRect.right - menuRect.width));
    const top = anchorRect.bottom + menuRect.height + 6 <= window.innerHeight ? anchorRect.bottom + 5 : Math.max(8, anchorRect.top - menuRect.height - 5);
    elements.rowActionMenu.style.left = `${left}px`;
    elements.rowActionMenu.style.top = `${top}px`;
  }

  function closeRowActionMenu() {
    state.rowActionAnchor?.setAttribute('aria-expanded', 'false');
    state.rowActionAnchor = null;
    if (elements.rowActionMenu) elements.rowActionMenu.hidden = true;
  }

  function toggleMulti(item, row, checked, event, index) {
    if (event.shiftKey && state.lastSelectedIndex >= 0) {
      const [start, end] = [state.lastSelectedIndex, index].sort((a, b) => a - b);
      state.visibleItems.slice(start, end + 1).forEach(candidate => state.selectedPaths.add(candidate.path));
    } else if (checked) state.selectedPaths.add(item.path);
    else state.selectedPaths.delete(item.path);
    state.lastSelectedIndex = index;
    qsa('.file-row-v2', elements.list).forEach(current => {
      const selected = state.selectedPaths.has(current.dataset.path);
      current.classList.toggle('multi-selected', selected);
      const input = qs('input[type="checkbox"]', current); if (input) input.checked = selected;
    });
    updateBatchBar();
  }

  function clearMultiSelection() {
    state.selectedPaths.clear();
    state.lastSelectedIndex = -1;
    qsa('.file-row-v2.multi-selected', elements.list).forEach(row => row.classList.remove('multi-selected'));
    qsa('.selection-cell input:checked', elements.list).forEach(input => { input.checked = false; });
    updateBatchBar();
  }

  function updateBatchBar() {
    const count = state.selectedPaths.size;
    elements.batchActions.hidden = count === 0;
    elements.batchCount.textContent = `已选择 ${count} 项`;
    if (elements.selectAll) {
      elements.selectAll.checked = count > 0 && count === state.visibleItems.length;
      elements.selectAll.indeterminate = count > 0 && count < state.visibleItems.length;
    }
    if (count > 0) elements.selectionActions.hidden = true;
  }

  function renderEmpty(title, description) {
    const empty = document.createElement('div');
    empty.className = 'empty-workspace';
    const inner = document.createElement('div');
    const strong = document.createElement('strong');
    const small = document.createElement('span');
    strong.textContent = title;
    small.textContent = description;
    inner.append(strong, small);
    empty.append(inner);
    elements.list.replaceChildren(empty);
  }

  function setSelected(item, row) {
    qsa('.file-row-v2.selected', elements.list).forEach(node => node.classList.remove('selected'));
    if (row) row.classList.add('selected');
    state.selected = item;
    state.details = item;
    elements.selectionActions.hidden = false;
    elements.contextLabel.textContent = item.type === 'dir' ? '所选目录' : '所选文件';
    updateActionBar(item);
  }

  async function showPreview(item) {
    try {
      const details = await api(`/api/item?path=${encodeURIComponent(item.path)}`);
      clearMultiSelection();
      const listRow = qsa('.file-row-v2', elements.list).find(row => row.dataset.path === details.path);
      setSelected(details, listRow);
      markTreeSelection(details.path);
      state.sourceMode = false;
      elements.directoryView.hidden = true;
      elements.previewView.hidden = false;
      elements.previewTitle.textContent = details.name;
      elements.previewMeta.textContent = `${labelForType(details.preview)} · ${details.size_label} · 更新于 ${details.modified}`;
      elements.sourceToggle.hidden = !['markdown', 'html'].includes(details.preview);
      elements.sourceToggle.textContent = '查看源码';
      elements.metadata.innerHTML = `<span>名称 <strong>${escapeHTML(details.name)}</strong></span><span>大小 <strong>${escapeHTML(details.size_label)}</strong></span><span>更新时间 <strong>${escapeHTML(details.modified)}</strong></span><span>路径 <strong>${escapeHTML(details.logical_path)}</strong></span>`;
      await renderPreview(details);
    } catch (error) {
      toast(error.message, true);
    }
  }

  async function renderPreview(item) {
    elements.previewStage.replaceChildren();
    if (state.sourceMode || item.preview === 'text') {
      const pre = document.createElement('pre');
      pre.textContent = '正在读取…';
      elements.previewStage.append(pre);
      try {
        const response = await fetch(item.content_url, { credentials: 'same-origin' });
        if (!response.ok) throw new Error('无法读取文件内容');
        pre.textContent = await response.text();
      } catch (error) { pre.textContent = error.message; }
      return;
    }
    if (['html', 'markdown', 'svg'].includes(item.preview)) {
      const frame = document.createElement('iframe');
      frame.title = `${item.name} 预览`;
      frame.referrerPolicy = 'no-referrer';
      frame.setAttribute('sandbox', item.preview === 'markdown' ? 'allow-same-origin' : 'allow-scripts allow-forms allow-modals allow-same-origin');
      frame.src = item.preview_url;
      elements.previewStage.append(frame);
      return;
    }
    if (item.preview === 'image') {
      const image = document.createElement('img'); image.alt = item.name; image.src = item.preview_url; elements.previewStage.append(image); return;
    }
    if (item.preview === 'pdf') {
      const frame = document.createElement('iframe'); frame.title = `${item.name} PDF 预览`; frame.src = item.preview_url; elements.previewStage.append(frame); return;
    }
    if (item.preview === 'video') {
      const video = document.createElement('video'); video.controls = true; video.src = item.preview_url; elements.previewStage.append(video); return;
    }
    if (item.preview === 'audio') {
      const audio = document.createElement('audio'); audio.controls = true; audio.src = item.preview_url; elements.previewStage.append(audio); return;
    }
    const empty = document.createElement('div'); empty.className = 'preview-empty'; empty.innerHTML = '<span>◇</span><strong>暂不支持在线预览</strong><small>可以下载后使用本地应用打开</small>'; elements.previewStage.append(empty);
  }

  function updateActionBar(item) {
    elements.logicalPath.textContent = item.logical_path;
    elements.shareToggle.checked = Boolean(item.shared);
    elements.copyShare.disabled = !item.shared || !item.share_url;
    elements.copyShare.dataset.copy = item.share_url || '';
    elements.copyPath.dataset.copy = item.logical_path;
    elements.download.hidden = item.type === 'dir';
    if (item.type !== 'dir') elements.download.href = item.content_url || `/content?path=${encodeURIComponent(item.path)}`;
    elements.edit.hidden = !item.editable;
  }

  function openDirectory(dir, options = {}) {
    elements.search.value = '';
    loadDirectory(dir);
    if (options.revealInTree !== false) ensureTreePath(dir);
  }

  async function loadTreeChildren(dir, container, force = false) {
    if (container.dataset.loaded === 'true' && !force) return;
    if (container.dataset.loaded === 'loading' && !force) return;
    const loadToken = String(++state.treeLoadToken);
    container.dataset.loadToken = loadToken;
    container.dataset.loaded = 'loading';
    container.setAttribute('aria-busy', 'true');
    container.innerHTML = '<div class="tree-loading">读取中…</div>';
    try {
      const result = await api(`/api/list?tree=1&path=${encodeURIComponent(dir)}`);
      if (container.dataset.loadToken !== loadToken) return;
      container.replaceChildren();
      result.items.forEach(item => container.append(createTreeNode(item)));
      container.dataset.loaded = 'true';
    } catch (error) {
      if (container.dataset.loadToken !== loadToken) return;
      container.innerHTML = '<div class="tree-loading">无法读取</div>';
      container.dataset.loaded = 'false';
    } finally {
      if (container.dataset.loadToken === loadToken) container.removeAttribute('aria-busy');
    }
  }

  function createTreeNode(item) {
    const wrapper = document.createElement('div');
    wrapper.role = 'none';
    const row = document.createElement('button');
    row.type = 'button'; row.className = 'tree-row'; row.dataset.treePath = item.path; row.dataset.type = item.type; row.role = 'treeitem';
    row.tabIndex = -1;
    row.setAttribute('aria-level', String(item.path.split('/').length + 1));
    const chevron = document.createElement('span'); chevron.className = 'tree-chevron'; chevron.textContent = item.type === 'dir' ? '⌄' : ''; chevron.setAttribute('aria-hidden', 'true');
    const icon = document.createElement('span'); icon.className = `tree-icon ${item.type === 'dir' ? 'folder-icon' : 'file-icon-css'}`; icon.setAttribute('aria-hidden', 'true');
    const name = document.createElement('span'); name.className = 'tree-name'; name.textContent = item.name;
    row.append(chevron, icon, name); wrapper.append(row);
    if (item.type === 'dir') {
      bindMoveDropTarget(row, item.path);
      row.setAttribute('aria-expanded', 'false');
      const children = document.createElement('div'); children.className = 'tree-children'; children.role = 'group'; children.hidden = true; wrapper.append(children);
    }
    bindTreeRow(row, item);
    return wrapper;
  }

  async function setTreeExpanded(row, expanded) {
    if (row.dataset.type !== 'dir') return;
    const children = row.nextElementSibling;
    if (!children?.classList.contains('tree-children')) return;
    row.setAttribute('aria-expanded', String(expanded));
    children.hidden = !expanded;
    if (expanded) await loadTreeChildren(row.dataset.treePath || '', children);
  }

  function bindTreeRow(row, item) {
    row.addEventListener('click', () => {
      setTreeFocus(row, false);
      if (item.type === 'dir') {
        setTreeExpanded(row, row.getAttribute('aria-expanded') !== 'true');
        if (state.currentDir !== item.path || elements.directoryView.hidden) openDirectory(item.path, { revealInTree: false });
        else markTreeSelection(item.path);
      } else {
        showPreview(item);
      }
    });
  }

  async function ensureTreePath(dir, options = {}) {
    if (!dir) return;
    await setTreeExpanded(qs('.root-row', elements.tree), true);
    const expandTarget = options.expandTarget !== false;
    let parent = '';
    const parts = dir.split('/');
    for (let index = 0; index < parts.length; index++) {
      const part = parts[index];
      const next = parent ? `${parent}/${part}` : part;
      const row = qsa('[data-tree-path]', elements.tree).find(node => node.dataset.treePath === next);
      if (!row) break;
      if (row.dataset.type === 'dir' && (index < parts.length - 1 || expandTarget)) await setTreeExpanded(row, true);
      parent = next;
    }
    if (options.markSelection !== false) markTreeSelection(dir);
  }

  function markTreeSelection(path) {
    qsa('.tree-row', elements.tree).forEach(row => {
      const selected = row.dataset.treePath === path;
      row.classList.toggle('selected', selected);
      row.setAttribute('aria-selected', String(selected));
    });
    const row = qsa('[data-tree-path]', elements.tree).find(node => node.dataset.treePath === path);
    if (row) setTreeFocus(row, false);
  }

  function initActions() {
    qs('#preview-back').addEventListener('click', () => {
      elements.previewView.hidden = true; elements.directoryView.hidden = false;
      elements.contextLabel.textContent = '当前位置'; elements.logicalPath.textContent = logicalPath(state.currentDir);
      state.sourceMode = false;
    });
    elements.search.addEventListener('input', renderList);
    qsa('[data-sort]').forEach(button => button.addEventListener('click', () => {
      const key = button.dataset.sort;
      if (state.sort === key) state.sortDirection *= -1; else { state.sort = key; state.sortDirection = 1; }
      renderList();
    }));
    elements.copyShare.addEventListener('click', () => copyText(elements.copyShare.dataset.copy, elements.copyShare));
    elements.copyPath.addEventListener('click', () => copyText(elements.copyPath.dataset.copy, elements.copyPath));
    elements.shareToggle.addEventListener('change', toggleShare);
    elements.sourceToggle.addEventListener('click', async () => {
      state.sourceMode = !state.sourceMode; elements.sourceToggle.textContent = state.sourceMode ? '返回预览' : '查看源码'; await renderPreview(state.details);
    });
    elements.edit.addEventListener('click', openEditor);
    elements.rename.addEventListener('click', openRename);
    elements.move.addEventListener('click', () => openMove([state.details?.path].filter(Boolean)));
    elements.remove.addEventListener('click', removeSelected);
    elements.selectAll.addEventListener('change', () => {
      state.selectedPaths.clear();
      if (elements.selectAll.checked) state.visibleItems.forEach(item => state.selectedPaths.add(item.path));
      qsa('.file-row-v2', elements.list).forEach(row => {
        const selected = state.selectedPaths.has(row.dataset.path); row.classList.toggle('multi-selected', selected); qs('input[type="checkbox"]', row).checked = selected;
      });
      updateBatchBar();
    });
    qs('#batch-clear').addEventListener('click', clearMultiSelection);
    qs('#batch-download').addEventListener('click', () => downloadBatch([...state.selectedPaths]));
    qs('#batch-move').addEventListener('click', () => openMove([...state.selectedPaths]));
    qs('#batch-delete').addEventListener('click', removeBatch);
    qs('#row-edit-file').addEventListener('click', () => { closeRowActionMenu(); openEditor(); });
    qs('#row-rename-file').addEventListener('click', () => { closeRowActionMenu(); openRename(); });
    qs('#row-move-file').addEventListener('click', () => { closeRowActionMenu(); openMove([state.details?.path].filter(Boolean)); });
    qs('#row-delete-file').addEventListener('click', () => { closeRowActionMenu(); removeSelected(); });
    qs('.file-table').addEventListener('scroll', closeRowActionMenu);
    window.addEventListener('resize', closeRowActionMenu);
    qs('#tree-refresh').addEventListener('click', refreshWorkspace);
    window.addEventListener('popstate', event => {
      const path = event.state?.path || new URL(location.href).searchParams.get('path') || '';
      loadDirectory(path, { history: false });
      ensureTreePath(path);
    });

    qsa('[data-new-kind]').forEach(button => button.addEventListener('click', () => openNew(button.dataset.newKind)));
    qs('#new-form').addEventListener('submit', createItem);
    qs('#rename-form').addEventListener('submit', renameSelected);
    qs('#editor-form').addEventListener('submit', saveEditor);
    qs('#move-form').addEventListener('submit', moveSelected);
    initCommandPalette();
  }

  async function toggleShare() {
    if (!state.details) return;
    const desired = elements.shareToggle.checked;
    elements.shareToggle.disabled = true;
    try {
      const result = await post('/api/share', { path: state.details.path, enabled: String(desired) });
      state.details.shared = result.shared; state.details.share_url = result.share_url;
      const item = state.items.find(candidate => candidate.path === state.details.path);
      if (item) { item.shared = result.shared; item.share_url = result.share_url; }
      updateActionBar(state.details); renderList(); toast(desired ? '分享链接已开启' : '分享链接已关闭');
    } catch (error) {
      elements.shareToggle.checked = !desired; toast(error.message, true);
    } finally { elements.shareToggle.disabled = false; }
  }

  function openNew(kind) {
    qs('#new-menu').open = false;
    const titles = { folder: '新建文件夹', markdown: '新建 Markdown', html: '新建 HTML 页面', text: '新建纯文本' };
    const placeholders = { folder: '文件夹名称', markdown: '例如 传阅说明.md', html: '例如 index.html', text: '例如 说明.txt' };
    qs('#new-kind').value = kind; qs('#new-dialog-title').textContent = titles[kind]; qs('#new-name').placeholder = placeholders[kind]; qs('#new-name').value = '';
    qs('#new-dialog').showModal(); setTimeout(() => qs('#new-name').focus(), 0);
  }

  async function createItem(event) {
    event.preventDefault();
    const submit = event.submitter; submit.disabled = true;
    try {
      const result = await post('/api/new', { path: state.currentDir, kind: qs('#new-kind').value, name: qs('#new-name').value });
      qs('#new-dialog').close(); toast('创建成功'); await refreshWorkspace();
      if (result.type === 'file') {
        const item = state.items.find(candidate => candidate.path === result.path); if (item) showPreview(item);
      }
    } catch (error) { toast(error.message, true); } finally { submit.disabled = false; }
  }

  function openRename() {
    if (!state.details) return;
    qs('#file-actions').open = false; qs('#rename-name').value = state.details.name; qs('#rename-dialog').showModal(); setTimeout(() => qs('#rename-name').select(), 0);
  }

  async function renameSelected(event) {
    event.preventDefault();
    if (!state.details) return;
    try {
      await post('/rename', { path: state.details.path, name: qs('#rename-name').value }); qs('#rename-dialog').close(); toast('已重命名'); await refreshWorkspace();
    } catch (error) { toast(error.message, true); }
  }

  async function removeSelected() {
    if (!state.details) return;
    qs('#file-actions').open = false;
    const confirmed = await confirmAction(`删除“${state.details.name}”？`, state.details.type === 'dir' ? '目录内的全部文件及相关分享链接都会一并删除。' : '文件及其分享链接将被删除，此操作无法撤销。');
    if (!confirmed) return;
    try {
      await post('/delete', { path: state.details.path }); toast('已删除'); elements.previewView.hidden = true; elements.directoryView.hidden = false; await refreshWorkspace();
    } catch (error) { toast(error.message, true); }
  }

  async function removeBatch() {
    const paths = [...state.selectedPaths];
    if (!paths.length) return;
    const confirmed = await confirmAction(`删除所选 ${paths.length} 项？`, '所选文件、目录及相关分享链接都会一并删除，此操作无法撤销。');
    if (!confirmed) return;
    try {
      await post('/api/delete-batch', { paths: JSON.stringify(paths) }); toast(`已删除 ${paths.length} 项`); clearMultiSelection(); await refreshWorkspace();
    } catch (error) { toast(error.message, true); }
  }

  async function openMove(paths) {
    if (!paths.length) return;
    qs('#file-actions').open = false;
    try {
      const result = await api('/api/directories');
      const select = qs('#move-destination'); select.replaceChildren();
      result.directories.forEach(directory => {
        const option = document.createElement('option'); option.value = directory.path; option.textContent = directory.logical_path; select.append(option);
      });
      qs('#move-summary').textContent = paths.length === 1 ? `移动“${paths[0].split('/').pop()}”` : `移动所选 ${paths.length} 项`;
      qs('#move-form').dataset.paths = JSON.stringify(paths); qs('#move-dialog').showModal();
    } catch (error) { toast(error.message, true); }
  }

  async function moveSelected(event) {
    event.preventDefault();
    const paths = qs('#move-form').dataset.paths;
    event.submitter.disabled = true;
    try {
      const result = await movePaths(JSON.parse(paths), qs('#move-destination').value); qs('#move-dialog').close(); toast(`已移动 ${result.moved} 项`);
    } catch (error) { toast(error.message, true); } finally { event.submitter.disabled = false; }
  }

  async function movePaths(paths, destination) {
    const result = await post('/api/move', { paths: JSON.stringify(paths), destination });
    clearMultiSelection();
    await refreshWorkspace();
    return result;
  }

  function bindDragSource(row, item) {
    row.addEventListener('dragstart', event => {
      const selectedPaths = state.selectedPaths.has(item.path) ? [...state.selectedPaths] : [item.path];
      state.dragPaths = selectedPaths;
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('application/x-filehub-paths', JSON.stringify(selectedPaths));
      event.dataTransfer.setData('text/plain', selectedPaths.length === 1 ? item.name : `${selectedPaths.length} 个项目`);
      document.body.classList.add('file-drag-active');
      row.classList.add('dragging-source');
    });
    row.addEventListener('dragend', clearFileDragState);
  }

  function bindMoveDropTarget(target, destination) {
    target.dataset.dropDestination = destination;
    target.addEventListener('dragenter', event => {
      if (!canDropFiles(destination)) return;
      event.preventDefault();
      target.classList.add('move-drop-target');
    });
    target.addEventListener('dragover', event => {
      if (!canDropFiles(destination)) return;
      event.preventDefault();
      event.stopPropagation();
      event.dataTransfer.dropEffect = 'move';
      target.classList.add('move-drop-target');
    });
    target.addEventListener('dragleave', event => {
      if (event.relatedTarget && target.contains(event.relatedTarget)) return;
      target.classList.remove('move-drop-target');
    });
    target.addEventListener('drop', async event => {
      if (!canDropFiles(destination)) return;
      event.preventDefault();
      event.stopPropagation();
      const paths = [...state.dragPaths];
      clearFileDragState();
      try {
        const result = await movePaths(paths, destination);
        toast(`已拖拽移动 ${result.moved} 项`);
      } catch (error) { toast(error.message, true); }
    });
  }

  function canDropFiles(destination) {
    return state.dragPaths.length > 0 && state.dragPaths.every(source => {
      const parent = source.includes('/') ? source.slice(0, source.lastIndexOf('/')) : '';
      return destination !== parent && destination !== source && !destination.startsWith(`${source}/`);
    });
  }

  function clearFileDragState() {
    state.dragPaths = [];
    document.body.classList.remove('file-drag-active');
    qsa('.dragging-source, .move-drop-target').forEach(node => node.classList.remove('dragging-source', 'move-drop-target'));
  }

  function downloadBatch(paths) {
    if (!paths.length) return;
    const query = new URLSearchParams(); paths.forEach(item => query.append('path', item));
    const link = document.createElement('a'); link.href = `/api/download?${query}`; link.download = ''; document.body.append(link); link.click(); link.remove(); toast('正在准备压缩包');
  }

  async function openEditor() {
    if (!state.details?.editable) return;
    qs('#file-actions').open = false;
    try {
      const response = await fetch(state.details.content_url, { credentials: 'same-origin' });
      if (!response.ok) throw new Error('无法读取文件内容');
      qs('#editor-title').textContent = `编辑 ${state.details.name}`; qs('#editor-content').value = await response.text(); qs('#editor-dialog').showModal();
      setTimeout(() => qs('#editor-content').focus(), 0);
    } catch (error) { toast(error.message, true); }
  }

  async function saveEditor(event) {
    event.preventDefault();
    if (!state.details) return;
    event.submitter.disabled = true;
    try {
      await post('/api/save', { path: state.details.path, content: qs('#editor-content').value }); qs('#editor-dialog').close(); toast('文件已保存');
      const refreshed = await api(`/api/item?path=${encodeURIComponent(state.details.path)}`); state.details = refreshed; await renderPreview(refreshed); await loadDirectory(state.currentDir, { history: false }); await showPreview(refreshed);
    } catch (error) { toast(error.message, true); } finally { event.submitter.disabled = false; }
  }

  async function refreshWorkspace() {
    const treeState = new Map(qsa('.tree-row[aria-expanded]', elements.tree).map(row => [row.dataset.treePath || '', row.getAttribute('aria-expanded') === 'true']));
    await loadDirectory(state.currentDir, { history: false });
    await loadTreeChildren('', elements.treeRoot, true);
    const expandedPaths = [...treeState].filter(([, expanded]) => expanded).map(([path]) => path).filter(Boolean).sort((a, b) => a.split('/').length - b.split('/').length);
    for (const path of expandedPaths) await ensureTreePath(path, { markSelection: false });
    [...treeState].filter(([path, expanded]) => path && !expanded).forEach(([path]) => {
      const row = qsa('[data-tree-path]', elements.tree).find(node => node.dataset.treePath === path);
      if (row) setTreeExpanded(row, false);
    });
    await ensureTreePath(state.currentDir, { expandTarget: treeState.get(state.currentDir) === true });
    await setTreeExpanded(qs('.root-row', elements.tree), treeState.get('') !== false);
    markTreeSelection(state.currentDir);
  }

  async function initializeWorkspace() {
    await Promise.all([
      loadDirectory(state.currentDir, { history: false }),
      loadTreeChildren('', elements.treeRoot, true),
    ]);
    await ensureTreePath(state.currentDir);
  }

  function initTree() {
    const rootRow = qs('.root-row', elements.tree);
    rootRow.tabIndex = 0;
    rootRow.setAttribute('aria-level', '1');
    rootRow.setAttribute('aria-selected', String(state.currentDir === ''));
    qsa('.tree-chevron, .tree-icon', rootRow).forEach(node => node.setAttribute('aria-hidden', 'true'));
    bindTreeRow(rootRow, { path: '', type: 'dir' });
    bindMoveDropTarget(rootRow, '');

    elements.tree.addEventListener('keydown', async event => {
      const row = event.target.closest('.tree-row');
      if (!row || !elements.tree.contains(row)) return;
      const visibleRows = qsa('.tree-row', elements.tree).filter(candidate => !candidate.closest('.tree-children[hidden]'));
      const index = visibleRows.indexOf(row);
      let target;
      if (event.key === 'ArrowDown') target = visibleRows[Math.min(visibleRows.length - 1, index + 1)];
      else if (event.key === 'ArrowUp') target = visibleRows[Math.max(0, index - 1)];
      else if (event.key === 'Home') target = visibleRows[0];
      else if (event.key === 'End') target = visibleRows[visibleRows.length - 1];
      else if (event.key === 'ArrowRight' && row.dataset.type === 'dir') {
        event.preventDefault(); event.stopPropagation();
        if (row.getAttribute('aria-expanded') !== 'true') await setTreeExpanded(row, true);
        else target = qs(':scope > .tree-row, :scope > [role="none"] > .tree-row', row.nextElementSibling);
      } else if (event.key === 'ArrowLeft') {
        event.preventDefault(); event.stopPropagation();
        if (row.dataset.type === 'dir' && row.getAttribute('aria-expanded') === 'true') await setTreeExpanded(row, false);
        else target = parentTreeRow(row);
      } else if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault(); event.stopPropagation(); row.click(); return;
      } else return;
      event.preventDefault(); event.stopPropagation();
      if (target) setTreeFocus(target);
    });
  }

  function setTreeFocus(row, focus = true) {
    qsa('.tree-row', elements.tree).forEach(candidate => { candidate.tabIndex = candidate === row ? 0 : -1; });
    if (focus) row.focus();
  }

  function parentTreeRow(row) {
    const group = row.parentElement?.closest('.tree-children');
    if (!group) return null;
    return group.previousElementSibling?.classList.contains('tree-row') ? group.previousElementSibling : null;
  }

  function initUpload() {
    const form = qs('#upload-form'); const input = qs('#file-input'); const queue = qs('#upload-queue'); const progress = qs('.upload-progress', form); const bar = qs('i', progress);
    let mainDragDepth = 0;
    const carriesExternalFiles = event => !state.dragPaths.length && [...(event.dataTransfer?.types || [])].includes('Files');
    const showMainDropTarget = () => {
      elements.uploadDestination.textContent = logicalPath(state.currentDir);
      elements.uploadOverlay.hidden = false;
      elements.main.classList.add('upload-drop-active');
    };
    const hideMainDropTarget = () => {
      mainDragDepth = 0;
      elements.uploadOverlay.hidden = true;
      elements.main.classList.remove('upload-drop-active');
    };
    const folderInput = qs('#folder-input');
    const folderToggle = qs('.upload-folder-toggle');
    form.addEventListener('click', event => {
      if (event.target === folderToggle) { event.preventDefault(); event.stopPropagation(); folderInput && folderInput.click(); return; }
      if (event.target.tagName !== 'INPUT') input.click();
    });
    form.addEventListener('keydown', event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); input.click(); } });
    if (folderInput) folderInput.addEventListener('change', () => upload(inputToFileItems(folderInput.files)));
    ['dragenter', 'dragover'].forEach(type => form.addEventListener(type, event => { event.preventDefault(); if (state.dragPaths.length) { event.dataTransfer.dropEffect = 'none'; return; } form.classList.add('dragging'); }));
    ['dragleave', 'drop'].forEach(type => form.addEventListener(type, event => { event.preventDefault(); form.classList.remove('dragging'); }));
    form.addEventListener('drop', event => { if (state.dragPaths.length) return; event.preventDefault(); uploadFilesFromDataTransfer(event.dataTransfer); });
    input.addEventListener('change', () => upload(inputToFileItems(input.files)));
    elements.main.addEventListener('dragenter', event => {
      if (!carriesExternalFiles(event)) return;
      event.preventDefault();
      mainDragDepth += 1;
      showMainDropTarget();
    });
    elements.main.addEventListener('dragover', event => {
      if (!carriesExternalFiles(event)) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = 'copy';
      showMainDropTarget();
    });
    elements.main.addEventListener('dragleave', event => {
      if (!carriesExternalFiles(event)) return;
      mainDragDepth = Math.max(0, mainDragDepth - 1);
      if (!mainDragDepth) hideMainDropTarget();
    });
    elements.main.addEventListener('drop', event => {
      if (!carriesExternalFiles(event)) return;
      event.preventDefault();
      event.stopPropagation();
      hideMainDropTarget();
      uploadFilesFromDataTransfer(event.dataTransfer);
    });
    window.addEventListener('blur', hideMainDropTarget);
    async function uploadFilesFromDataTransfer(dt) {
      // Prefer the entry API so dropped folders keep their structure.
      const items = [...(dt.items || [])];
      if (items.length && items[0].webkitGetAsEntry) {
        const collected = [];
        for (const item of items) {
          const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
          if (entry) collected.push(...await traverseEntry(entry));
          else { const f = item.getAsFile && item.getAsFile(); if (f) collected.push({ file: f, relPath: f.name }); }
        }
        if (collected.length) return upload(collected);
      }
      upload(inputToFileItems(dt.files));
    }
    function inputToFileItems(fileList) {
      return [...fileList].map(file => ({ file, relPath: file.webkitRelativePath || file.name }));
    }
    function readEntries(reader) {
      return new Promise((resolve, reject) => {
        const all = [];
        const step = () => reader.readEntries(entries => {
          if (!entries.length) { resolve(all); return; }
          all.push(...entries); step();
        }, reject);
        step();
      });
    }
    async function traverseEntry(entry, prefix = '') {
      const results = [];
      if (entry.isFile) {
        const file = await new Promise((res, rej) => entry.file(res, rej));
        results.push({ file, relPath: (prefix ? prefix + '/' : '') + entry.name });
      } else if (entry.isDirectory) {
        const next = (prefix ? prefix + '/' : '') + entry.name;
        const children = await readEntries(entry.createReader());
        for (const child of children) results.push(...await traverseEntry(child, next));
      }
      return results;
    }
    async function upload(items) {
      if (!items?.length) return;
      const duplicates = items.filter(({ relPath }) => !relPath.includes('/') && state.items.some(item => item.type === 'file' && item.name === relPath));
      if (duplicates.length && !await confirmAction('覆盖同名文件？', `${duplicates.map(({ relPath }) => relPath).slice(0, 3).join('、')}${duplicates.length > 3 ? ` 等 ${duplicates.length} 个文件` : ''} 已存在。继续上传会替换原文件。`, '继续上传')) return;
      const data = new FormData(); data.append('csrf', config.csrf); data.append('path', state.currentDir); items.forEach(({ file, relPath }) => { data.append('paths', relPath); data.append('files', file); });
      const count = items.length; const hasFolder = items.some(({ relPath }) => relPath.includes('/'));
      queue.hidden = false; queue.textContent = `正在上传 ${count} 个文件${hasFolder ? '（含文件夹）' : ''}…`; progress.hidden = false; bar.style.width = '0%'; input.value = ''; if (folderInput) folderInput.value = '';
      const xhr = new XMLHttpRequest(); xhr.open('POST', '/upload'); xhr.setRequestHeader('Accept', 'application/json');
      xhr.upload.onprogress = event => { if (event.lengthComputable) { const value = Math.round(event.loaded / event.total * 100); bar.style.width = `${value}%`; queue.textContent = `正在上传 ${count} 个文件${hasFolder ? '（含文件夹）' : ''} · ${value}%`; } };
      xhr.onload = async () => {
        progress.hidden = true;
        if (xhr.status >= 200 && xhr.status < 300) { queue.textContent = `${count} 个文件上传完成`; toast('上传完成，分享已默认开启'); await refreshWorkspace(); setTimeout(() => { queue.hidden = true; }, 2200); }
        else { queue.textContent = '上传失败'; toast(readXHRError(xhr), true); }
      };
      xhr.onerror = () => { progress.hidden = true; queue.textContent = '上传失败'; toast('网络错误，上传失败', true); }; xhr.send(data);
    }
  }

  function initDivider() {
    const divider = qs('#workspace-divider');
    const saved = Number(localStorage.getItem('filehub.sidebarWidth'));
    if (saved >= 260 && saved <= 520) document.documentElement.style.setProperty('--sidebar-width', `${saved}px`);
    let dragging = false;
    const setWidth = clientX => { const width = Math.max(260, Math.min(520, clientX)); document.documentElement.style.setProperty('--sidebar-width', `${width}px`); return width; };
    divider.addEventListener('pointerdown', event => { dragging = true; divider.classList.add('dragging'); divider.setPointerCapture(event.pointerId); setWidth(event.clientX); });
    divider.addEventListener('pointermove', event => { if (dragging) setWidth(event.clientX); });
    divider.addEventListener('pointerup', event => { if (!dragging) return; dragging = false; divider.classList.remove('dragging'); const width = setWidth(event.clientX); localStorage.setItem('filehub.sidebarWidth', String(width)); });
    divider.addEventListener('dblclick', () => { document.documentElement.style.setProperty('--sidebar-width', '326px'); localStorage.removeItem('filehub.sidebarWidth'); });
    divider.addEventListener('keydown', event => { if (!['ArrowLeft', 'ArrowRight'].includes(event.key)) return; const current = parseInt(getComputedStyle(document.documentElement).getPropertyValue('--sidebar-width')) || 326; const width = Math.max(260, Math.min(520, current + (event.key === 'ArrowRight' ? 12 : -12))); document.documentElement.style.setProperty('--sidebar-width', `${width}px`); localStorage.setItem('filehub.sidebarWidth', String(width)); });
  }

  function initMobileSidebar() {
    const scrim = qs('#sidebar-scrim');
    const open = () => { workspace.classList.add('sidebar-open'); scrim.hidden = false; };
    const close = () => { workspace.classList.remove('sidebar-open'); scrim.hidden = true; };
    qs('#sidebar-open').addEventListener('click', open); qs('#sidebar-close').addEventListener('click', close); scrim.addEventListener('click', close);
  }

  function initMenus() {
    document.addEventListener('click', event => {
      qsa('details[open]').forEach(details => { if (!details.contains(event.target)) details.open = false; });
      if (!elements.rowActionMenu.hidden && !elements.rowActionMenu.contains(event.target) && !state.rowActionAnchor?.contains(event.target)) closeRowActionMenu();
    });
  }

  function initCommandPalette() {
    const dialog = qs('#command-dialog'); const input = qs('#command-input'); const results = qs('#command-results');
    const commands = [
      { label: '上传文件', meta: 'U', icon: '↑', run: () => qs('#file-input').click() },
      { label: '新建文件夹', meta: 'N', icon: '+', run: () => openNew('folder') },
      { label: '新建 Markdown', meta: '.md', icon: '+', run: () => openNew('markdown') },
      { label: '新建 HTML 页面', meta: '.html', icon: '+', run: () => openNew('html') },
      { label: '搜索当前目录', meta: '⌘K', icon: '⌕', run: () => { elements.directoryView.hidden = false; elements.previewView.hidden = true; elements.search.focus(); } },
      { label: '刷新文件目录', meta: 'R', icon: '↻', run: () => refreshWorkspace() },
    ];
    if (config.role === 'admin') commands.push({ label: '打开账号管理', meta: '', icon: '◎', run: () => { location.href = '/admin/users'; } });
    commands.push({ label: '打开分享记录', meta: '', icon: '↗', run: () => { location.href = '/shares'; } });

    function entries() {
      const files = state.items.map(item => ({ label: item.name, meta: item.type === 'dir' ? '目录' : labelForType(item.preview), icon: item.type === 'dir' ? '▰' : '◇', run: () => item.type === 'dir' ? openDirectory(item.path) : showPreview(item) }));
      return [...commands, ...files];
    }
    function renderCommands() {
      const query = input.value.trim().toLocaleLowerCase();
      const visible = entries().filter(item => item.label.toLocaleLowerCase().includes(query)).slice(0, 24);
      state.commandIndex = Math.max(0, Math.min(state.commandIndex, visible.length - 1));
      results.replaceChildren();
      if (!visible.length) { const empty = document.createElement('div'); empty.className = 'command-empty'; empty.textContent = '没有匹配的文件或命令'; results.append(empty); return; }
      visible.forEach((item, index) => {
        const button = document.createElement('button'); button.type = 'button'; button.className = `command-item${index === state.commandIndex ? ' active' : ''}`; button.role = 'option'; button.innerHTML = `<i>${escapeHTML(item.icon)}</i><span></span><small>${escapeHTML(item.meta)}</small>`; qs('span', button).textContent = item.label;
        button.addEventListener('click', () => execute(item)); results.append(button);
      });
      qs('.command-item.active', results)?.scrollIntoView({ block: 'nearest' });
    }
    function execute(item) { dialog.close(); input.value = ''; item.run(); }
    function executeActive() {
      const query = input.value.trim().toLocaleLowerCase(); const visible = entries().filter(item => item.label.toLocaleLowerCase().includes(query)).slice(0, 24); if (visible[state.commandIndex]) execute(visible[state.commandIndex]);
    }
    window.openFileHubCommandPalette = () => { state.commandIndex = 0; input.value = ''; renderCommands(); if (!dialog.open) dialog.showModal(); setTimeout(() => input.focus(), 0); };
    input.addEventListener('input', () => { state.commandIndex = 0; renderCommands(); });
    input.addEventListener('keydown', event => {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); const count = qsa('.command-item', results).length; if (!count) return; state.commandIndex = (state.commandIndex + (event.key === 'ArrowDown' ? 1 : -1) + count) % count; renderCommands(); }
      else if (event.key === 'Enter') { event.preventDefault(); executeActive(); }
      else if (event.key === 'Escape') { event.preventDefault(); dialog.close(); }
    });
  }

  function initKeyboard() {
    document.addEventListener('keydown', event => {
      if (event.defaultPrevented) return;
      const typing = /INPUT|TEXTAREA|SELECT/.test(event.target.tagName) || event.target.isContentEditable;
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); window.openFileHubCommandPalette?.(); return; }
      if (event.key === 'Escape') { closeRowActionMenu(); qsa('details[open]').forEach(details => { details.open = false; }); if (!elements.previewView.hidden) qs('#preview-back').click(); return; }
      if (typing || qs('dialog[open]')) return;
      if (event.key.toLowerCase() === 'u') { event.preventDefault(); qs('#file-input').click(); return; }
      if (event.key.toLowerCase() === 'n') { event.preventDefault(); qs('#new-menu').open = true; return; }
      if (event.key === 'F2' && state.details) { event.preventDefault(); openRename(); return; }
      if (event.key === 'Delete' && state.selectedPaths.size) { event.preventDefault(); removeBatch(); return; }
      if (event.key === 'Delete' && state.details) { event.preventDefault(); removeSelected(); return; }
      const rows = qsa('.file-row-v2', elements.list);
      if (!rows.length) return;
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault(); state.activeRow = Math.max(0, Math.min(rows.length - 1, state.activeRow + (event.key === 'ArrowDown' ? 1 : -1))); rows.forEach(row => row.tabIndex = -1); rows[state.activeRow].tabIndex = 0; rows[state.activeRow].focus(); rows[state.activeRow].click();
      } else if ((event.key === 'Enter' || event.key === ' ') && state.selected) {
        event.preventDefault(); state.selected.type === 'dir' ? openDirectory(state.selected.path) : showPreview(state.selected);
      }
    });
  }

  function confirmAction(title, message, confirmText = '删除') {
    const dialog = qs('#confirm-dialog'); qs('#confirm-title').textContent = title; qs('#confirm-message').textContent = message; qs('#confirm-submit').textContent = confirmText; dialog.showModal();
    return new Promise(resolve => dialog.addEventListener('close', () => resolve(dialog.returnValue === 'confirm'), { once: true }));
  }

  function updateUsage(used, quota) {
    elements.storageLabel.textContent = `${formatBytes(used)} / ${formatBytes(quota)}`;
    elements.storageMeter.style.width = `${quota > 0 ? Math.min(100, Math.round(used / quota * 100)) : 0}%`;
  }

  function toast(message, isError = false) {
    let region = qs('#toast-region');
    if (!region) {
      region = document.createElement('div');
      region.id = 'toast-region';
      region.className = 'toast-region';
      region.setAttribute('aria-live', 'polite');
      document.body.append(region);
    }
    const node = document.createElement('div'); node.className = `toast${isError ? ' error' : ''}`; node.textContent = message; region.append(node); setTimeout(() => node.remove(), 3200);
  }

  function copyTextLegacy(value) {
    const activeElement = document.activeElement;
    const textarea = document.createElement('textarea');
    textarea.value = value;
    textarea.readOnly = true;
    textarea.setAttribute('aria-hidden', 'true');
    textarea.style.position = 'fixed';
    textarea.style.left = '0';
    textarea.style.top = '0';
    textarea.style.width = '1px';
    textarea.style.height = '1px';
    textarea.style.opacity = '0';
    document.body.append(textarea);
    textarea.focus();
    textarea.select();
    textarea.setSelectionRange(0, textarea.value.length);
    let copied = false;
    try { copied = document.execCommand('copy'); }
    catch { copied = false; }
    textarea.remove();
    if (activeElement?.focus) {
      try { activeElement.focus({ preventScroll: true }); }
      catch { activeElement.focus(); }
    }
    return copied;
  }

  async function copyText(value, button) {
    if (!value) return;
    let copied = false;
    if (window.isSecureContext && navigator.clipboard?.writeText) {
      try { await navigator.clipboard.writeText(value); copied = true; }
      catch { copied = copyTextLegacy(value); }
    } else {
      copied = copyTextLegacy(value);
    }
    if (!copied) { toast('复制失败，请手动复制', true); return; }
    const old = button?.textContent;
    if (button) button.textContent = '已复制';
    toast('已复制到剪贴板');
    if (button) setTimeout(() => { button.textContent = old; }, 1200);
  }

  function logicalPath(rel) { return `/${config.username}${rel ? `/${rel}` : ''}`; }
  function formatBytes(value) { const units = ['B', 'KB', 'MB', 'GB', 'TB']; let size = Number(value || 0), index = 0; while (size >= 1024 && index < units.length - 1) { size /= 1024; index++; } return index ? `${size.toFixed(1)} ${units[index]}` : `${size} B`; }
  function labelForType(type) { return ({ html: 'HTML', markdown: 'Markdown', image: '图片', svg: 'SVG', pdf: 'PDF', video: '视频', audio: '音频', text: '文本', none: '文件' })[type] || '文件'; }
  function escapeHTML(value) { return String(value).replace(/[&<>'"]/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]); }
  function readXHRError(xhr) { try { return JSON.parse(xhr.responseText).error || '上传失败'; } catch { return xhr.responseText || '上传失败'; } }
})();
