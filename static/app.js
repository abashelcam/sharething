var timers = {};

function fmt(b) {
  if (b < 1024) return b + ' B';
  if (b < 1048576) return (b/1024).toFixed(1) + ' KB';
  if (b < 1073741824) return (b/1048576).toFixed(1) + ' MB';
  return (b/1073741824).toFixed(2) + ' GB';
}

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function toast(msg, type) {
  var t = document.createElement('div');
  t.className = 'toast ' + (type || 'ok');
  t.textContent = msg;
  document.getElementById('toasts').appendChild(t);
  setTimeout(function(){ t.remove(); }, 3500);
}

function timeLeft(iso) {
  var diff = new Date(iso) - Date.now();
  if (diff <= 0) return 'Expired';
  var h = Math.floor(diff / 3600000);
  var m = Math.floor((diff % 3600000) / 60000);
  var s = Math.floor((diff % 60000) / 1000);
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}

function dlURL(shareID, fname) {
  return location.origin + '/d/' + shareID + '/' + encodeURIComponent(fname);
}

function copy(text) {
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).then(function(){ toast('Copied!'); }).catch(function(){ fallbackCopy(text); });
  } else {
    fallbackCopy(text);
  }
}

function fallbackCopy(text) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); toast('Copied!'); } catch(e) { toast('Copy failed', 'err'); }
  document.body.removeChild(ta);
}

function renderShares(shares) {
  var el = document.getElementById('shares');
  Object.keys(timers).forEach(function(k){ clearInterval(timers[k]); });
  timers = {};

  if (!shares || shares.length === 0) {
    el.innerHTML = '<div class="empty">No active shares &mdash; upload files to create one.</div>';
    return;
  }

  shares.sort(function(a,b){ return new Date(a.expires_at) - new Date(b.expires_at); });

  el.innerHTML = shares.map(function(sh) {
    var files = sh.files || [];
    var links = sh.links || [];
    var totalItems = files.length + links.length;

    var filesHTML = files.map(function(f) {
      var url = dlURL(sh.id, f.name);
      return '<div class="file-row">' +
        '<a class="file-name" href="' + url + '" title="' + esc(f.name) + '">' + esc(f.name) + '</a>' +
        '<span class="file-sz">' + fmt(f.size) + '</span>' +
        '<a class="btn btn-ghost btn-sm" href="' + esc(url) + '" target="_blank" rel="noopener">Open</a>' +
        '<button class="btn btn-ghost btn-sm file-copy" onclick="copy(\'' + url.replace(/'/g,"\\'") + '\')">Copy link</button>' +
        '</div>';
    }).join('');

    var linksHTML = links.map(function(l) {
      var label = esc(l.title || l.url);
      var safeURL = esc(l.url);
      var safeURLcopy = l.url.replace(/'/g,"\\'");
      return '<div class="link-item">' +
        '<span style="color:var(--muted);font-size:12px">&#128279;</span>' +
        '<a class="link-disp" href="' + safeURL + '" target="_blank" rel="noopener" title="' + safeURL + '">' + label + '</a>' +
        '<button class="btn btn-ghost btn-sm file-copy" onclick="copy(\'' + safeURLcopy + '\')">Copy</button>' +
        '</div>';
    }).join('');

    var metaLabel = [];
    if (files.length) metaLabel.push(files.length + ' file' + (files.length !== 1 ? 's' : ''));
    if (links.length) metaLabel.push(links.length + ' link' + (links.length !== 1 ? 's' : ''));

    return '<div class="share" id="sh-' + sh.id + '">' +
      '<div class="share-top">' +
      '<div style="flex:1;min-width:0">' +
      '<div class="share-id">' + esc(sh.id) + '</div>' +
      filesHTML + linksHTML +
      '</div>' +
      '<button class="btn btn-danger btn-sm" onclick="delShare(\'' + sh.id + '\')" title="Delete share">&#10005;</button>' +
      '</div>' +
      '<div class="share-foot">' +
      '<span class="expiry" id="ex-' + sh.id + '">...</span>' +
      '<span class="share-meta">' + metaLabel.join(' &bull; ') + ' &bull; ' + new Date(sh.created_at).toLocaleTimeString() + '</span>' +
      '</div>' +
      '</div>';
  }).join('');

  shares.forEach(function(sh) {
    function tick() {
      var el = document.getElementById('ex-' + sh.id);
      if (!el) { clearInterval(timers[sh.id]); return; }
      var diff = new Date(sh.expires_at) - Date.now();
      el.textContent = 'Expires in: ' + timeLeft(sh.expires_at);
      if (diff < 3600000) el.classList.add('urgent'); else el.classList.remove('urgent');
      if (diff <= 0) { clearInterval(timers[sh.id]); loadShares(); }
    }
    tick();
    timers[sh.id] = setInterval(tick, 1000);
  });
}

function loadShares() {
  fetch('/api/shares').then(function(r){ return r.json(); }).then(renderShares).catch(function() {
    document.getElementById('shares').innerHTML = '<div class="empty">Failed to load shares.</div>';
  });
}

function delShare(id) {
  if (!confirm('Delete this share and its files?')) return;
  fetch('/api/shares/' + id, {method:'DELETE'}).then(function(r) {
    if (r.ok) { toast('Share deleted'); loadShares(); }
    else toast('Delete failed', 'err');
  }).catch(function(){ toast('Delete failed', 'err'); });
}

// Upload via XHR for progress reporting
var zone = document.getElementById('zone');
var finput = document.getElementById('file-input');

zone.addEventListener('click', function(){ finput.click(); });
zone.addEventListener('dragover', function(e){ e.preventDefault(); zone.classList.add('over'); });
zone.addEventListener('dragleave', function(){ zone.classList.remove('over'); });
zone.addEventListener('drop', function(e){ e.preventDefault(); zone.classList.remove('over'); upload(e.dataTransfer.files); });
finput.addEventListener('change', function(){ upload(finput.files); });

function upload(files) {
  if (!files || !files.length) return;
  var form = new FormData();
  for (var i = 0; i < files.length; i++) form.append('files', files[i]);

  var prog = document.getElementById('prog');
  var bar = document.getElementById('prog-bar');
  var label = document.getElementById('prog-label');
  prog.classList.add('show');
  bar.style.width = '0';
  label.textContent = 'Uploading...';

  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/upload');

  xhr.upload.addEventListener('progress', function(e) {
    if (e.lengthComputable) {
      var pct = Math.round(e.loaded / e.total * 100);
      bar.style.width = pct + '%';
      label.textContent = pct + '% — ' + fmt(e.loaded) + ' / ' + fmt(e.total);
    }
  });

  xhr.addEventListener('load', function() {
    prog.classList.remove('show');
    finput.value = '';
    if (xhr.status === 200) {
      toast('Share created successfully!');
      loadShares();
    } else {
      toast(xhr.responseText || 'Upload failed', 'err');
    }
  });

  xhr.addEventListener('error', function() {
    prog.classList.remove('show');
    toast('Upload failed', 'err');
  });

  xhr.send(form);
}

// Settings
function openSettings() {
  fetch('/api/settings').then(function(r){ return r.json(); }).then(function(d) {
    document.getElementById('s-port').value = d.config.port;
    document.getElementById('s-max').value = d.config.max_file_size_mb;
    document.getElementById('s-exp').value = d.config.share_expiry_hours;
    document.getElementById('s-dir').value = d.config.storage_dir;
    document.getElementById('s-addr').textContent = d.ip + ':' + d.config.port;
    document.getElementById('s-disk').textContent = fmt(d.disk_usage_bytes);
  }).catch(function(){});
  document.getElementById('modal-overlay').classList.add('show');
}

function closeSettings() {
  document.getElementById('modal-overlay').classList.remove('show');
}

function saveSettings() {
  var cfg = {
    port: parseInt(document.getElementById('s-port').value) || 8080,
    max_file_size_mb: parseInt(document.getElementById('s-max').value) || 500,
    share_expiry_hours: parseInt(document.getElementById('s-exp').value) || 24,
    storage_dir: document.getElementById('s-dir').value || 'uploads'
  };
  fetch('/api/settings', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify(cfg)
  }).then(function(r){
    if (r.ok) {
      document.getElementById('max-mb').textContent = cfg.max_file_size_mb;
      toast('Settings saved');
      closeSettings();
    } else {
      toast('Failed to save', 'err');
    }
  }).catch(function(){ toast('Failed to save', 'err'); });
}

document.getElementById('modal-overlay').addEventListener('click', function(e) {
  if (e.target === this) closeSettings();
});

// Link share
function addLinkRow() {
  var row = document.createElement('div');
  row.className = 'link-entry';
  row.innerHTML = '<input type="url" class="link-url" placeholder="https://example.com">' +
    '<input type="text" class="link-title" placeholder="Label (optional)">' +
    '<button class="btn btn-ghost btn-sm" onclick="removeLinkRow(this)" title="Remove">&#10005;</button>';
  document.getElementById('link-rows').appendChild(row);
}

function removeLinkRow(btn) {
  var rows = document.querySelectorAll('#link-rows .link-entry');
  if (rows.length <= 1) { btn.closest('.link-entry').querySelector('.link-url').value = ''; return; }
  btn.closest('.link-entry').remove();
}

function createLinkShare() {
  var entries = document.querySelectorAll('#link-rows .link-entry');
  var links = [];
  for (var i = 0; i < entries.length; i++) {
    var url = entries[i].querySelector('.link-url').value.trim();
    var title = entries[i].querySelector('.link-title').value.trim();
    if (!url) continue;
    if (!url.startsWith('http://') && !url.startsWith('https://')) {
      toast('URLs must start with http:// or https://', 'err'); return;
    }
    links.push({url: url, title: title});
  }
  if (!links.length) { toast('Enter at least one URL', 'err'); return; }

  fetch('/api/links', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({links: links})
  }).then(function(r) {
    if (r.ok) {
      toast('Link share created!');
      // reset form
      var rows = document.getElementById('link-rows');
      rows.innerHTML = '<div class="link-entry">' +
        '<input type="url" class="link-url" placeholder="https://example.com">' +
        '<input type="text" class="link-title" placeholder="Label (optional)">' +
        '<button class="btn btn-ghost btn-sm" onclick="removeLinkRow(this)" title="Remove">&#10005;</button>' +
        '</div>';
      document.getElementById('link-panel').removeAttribute('open');
      loadShares();
    } else {
      r.text().then(function(t){ toast(t || 'Failed to create share', 'err'); });
    }
  }).catch(function(){ toast('Failed to create share', 'err'); });
}

// Init
loadShares();
setInterval(loadShares, 30000);
