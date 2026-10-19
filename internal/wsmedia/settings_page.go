package wsmedia

import "strings"

const awdlSetting = `<label class="check"><input data-path="DisableAWDL" type="checkbox"> Disable AWDL while streaming</label>`

// SettingsPageForPlatform uses the server platform, not the browser platform.
func SettingsPageForPlatform(platform string) string {
	if platform == "darwin" {
		return SettingsPage
	}
	return strings.Replace(SettingsPage, awdlSetting, "", 1)
}

// SettingsPage is the configuration editor served by the application handler.
const SettingsPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>AnyKVM settings</title><style>
:root{
  color-scheme:dark
}
[hidden]{
  display:none!important
}
body{
  max-width:1120px;
  margin:2rem auto;
  padding:0 1rem 4rem;
  background:#101318;
  color:#e8edf2;
  font:16px system-ui,sans-serif
}
h1{
  margin-bottom:.25rem
}
h2{
  font-size:18px;
  margin:0
}
h3{
  font-size:16px;
  margin:0
}
p{
  color:#b5c0cb;
  line-height:1.45
}
.nav{
  display:flex;
  gap:.5rem;
  overflow:auto;
  margin:1.5rem 0;
  border-bottom:1px solid #394451;
  padding-bottom:.65rem
}
.nav button{
  margin:0;
  white-space:nowrap;
  background:transparent;
  color:#b5c0cb
}
.nav button.active{
  background:#4d8df7;
  color:white
}
.grid{
  display:grid;
  grid-template-columns:repeat(auto-fit,minmax(280px,1fr));
  gap:1rem
}
.card{
  border:1px solid #394451;
  border-radius:10px;
  background:#171d25;
  padding:1.15rem;
  margin-bottom:1rem
}
.fields{
  display:grid;
  gap:.8rem;
  margin-top:1rem;
  container-type:inline-size
}
label{
  display:grid;
  gap:.35rem;
  color:#cdd7e2;
  font-size:14px
}
input,select,textarea{
  box-sizing:border-box;
  width:100%;
  padding:.6rem .65rem;
  border:1px solid #44515e;
  border-radius:6px;
  background:#10151c;
  color:#eef3f8;
  font:inherit
}
input:not([type=checkbox]):not([type=radio]),select{
  height:2.65rem
}
input:focus,select:focus,textarea:focus{
  outline:2px solid #4d8df7;
  outline-offset:1px
}
input[type=checkbox],input[type=radio]{
  width:auto
}
.check{
  display:flex;
  align-items:center;
  align-self:center;
  gap:.55rem
}
.check input{
  margin:0
}
button{
  margin-top:1rem;
  padding:.65rem 1rem;
  border:0;
  border-radius:6px;
  background:#4d8df7;
  color:white;
  font-weight:600;
  cursor:pointer
}
button.secondary{
  background:#354454
}
button.saveOnly{
  background:#2f855a
}
button.danger{
  background:#a94848
}
button:disabled{
  opacity:.6;
  cursor:wait
}
.hint{
  margin:.35rem 0 0;
  font-size:14px
}
.warning{
  padding:.75rem 1rem;
  border-left:3px solid #f1b64c;
  background:#272116
}
.serviceHead,.targetHead{
  display:flex;
  align-items:center;
  gap:.75rem
}
.serviceHead .check,.sectionHead .check{
  margin-left:auto
}
.webdavPermissions{
  display:flex;
  gap:1.5rem;
  flex-wrap:nowrap
}
.webdavPermissions .check{
  white-space:nowrap
}
.clipboardPermissions{
  grid-column:1/-1;
  width:100%;
  table-layout:fixed;
  border-collapse:collapse;
  color:#cdd7e2;
  font-size:14px
}
.targetChoices{
  grid-column:1/-1;
  display:grid;
  grid-template-columns:repeat(auto-fit,minmax(min(100%,260px),1fr));
  gap:.8rem 1rem
}
.targetChoiceRow{
  display:flex;
  align-items:center;
  gap:.75rem;
  min-width:0;
  color:#cdd7e2;
  font-size:clamp(11px,2.8vw,14px)
}
.targetChoiceRow>span{
  flex:0 0 3.5em
}
.targetMouseGroup{
  grid-column:1/-1
}
.wheelScale{
  grid-column:1/-1;
  display:flex;
  align-items:center;
  gap:.75rem
}
.wheelScale>span{
  flex:none;
  white-space:nowrap
}
.wheelScale>input{
  flex:1;
  min-width:0
}
.wheelScale>output{
  flex:0 0 3.5em;
  text-align:right;
  font-variant-numeric:tabular-nums
}
.rangeHeading{
  display:flex;
  align-items:center;
  justify-content:space-between
}
.rangeHeading output{
  font-variant-numeric:tabular-nums
}
input[type=range]{
  padding:0;
  border:0;
  background:transparent;
  appearance:none;
  -webkit-appearance:none
}
input[type=range]::-webkit-slider-runnable-track{
  height:6px;
  border-radius:3px;
  background:var(--wheel-track,#44515e)
}
input[type=range]::-moz-range-track{
  height:6px;
  border-radius:3px;
  background:var(--wheel-track,#44515e)
}
input[type=range]::-moz-range-progress{
  background:transparent
}
input[type=range]::-webkit-slider-thumb{
  appearance:none;
  -webkit-appearance:none;
  width:16px;
  height:16px;
  margin-top:-5px;
  border-radius:50%;
  background:#4d8df7;
  border:0
}
input[type=range]::-moz-range-thumb{
  width:16px;
  height:16px;
  border-radius:50%;
  background:#4d8df7;
  border:0
}
.targetMouseGroup h4{
  font-size:14px;
  margin:.3rem 0 0;
  color:#cdd7e2
}
.targetMouseGroup .fields{
  margin-top:.7rem
}
.radioOptions{
  display:grid;
  grid-template-columns:6.5em 6.5em;
  align-items:center;
  gap:.5rem
}
.radioOptions .check{
  white-space:nowrap;
  font-size:inherit
}
.clipboardPermissions th,.clipboardPermissions td{
  padding:.4rem .2rem;
  text-align:center
}
.clipboardPermissions th{
  font-weight:400
}
.clipboardPermissions th:first-child{
  width:28%;
  text-align:left
}
.clipboardPermissions thead th{
  font-size:clamp(10px,2.6vw,13px);
  white-space:nowrap;
  color:#b5c0cb
}
.clipboardPermissions input{
  margin:0;
  vertical-align:middle
}
.clipboardPermissions tbody tr+tr{
  border-top:1px solid #29333f
}
.sectionHead{
  grid-column:1/-1;
  display:flex;
  align-items:center;
  gap:.75rem
}
.serviceBody{
  margin-top:1rem;
  padding-top:1rem;
  border-top:1px solid #394451
}
.codecOptions{
  grid-column:1/-1;
  display:flex;
  flex-wrap:nowrap;
  gap:.35rem
}
.codecOptions .check{
  flex:1 0 auto;
  min-width:0;
  gap:.25rem;
  white-space:nowrap;
  font-size:clamp(10px,2.8vw,14px)
}
.codecOptions input{
  flex-shrink:0
}
.serviceBody h3{
  grid-column:1/-1
}
.targetLayout{
  display:grid;
  grid-template-columns:290px minmax(0,1fr);
  gap:1rem
}
.targetList{
  display:grid;
  gap:.6rem;
  align-content:start
}
.targetCard{
  display:block;
  margin:0;
  padding:.85rem;
  text-align:left;
  background:#202a35;
  border:1px solid transparent
}
.targetCard.active{
  border-color:#4d8df7;
  background:#253955
}
.targetGroup{
  margin-top:1.25rem;
  padding-top:1rem;
  border-top:1px solid #394451
}
.targetDetail{
  min-height:280px
}
.detailActions{
  display:flex;
  gap:.6rem;
  align-items:center;
  margin-top:.8rem
}
.detailActions button{
  margin:0
}
.saveRow{
  display:flex;
  align-items:center;
  gap:1rem;
  margin-top:1.5rem
}
#status{
  color:#b5c0cb
}
textarea{
  min-height:62vh;
  font:13px ui-monospace,SFMono-Regular,monospace;
  line-height:1.45
}
@container(min-width:576px){
  .fields>.check{
    align-self:stretch;
    padding-top:calc(1.2em + .35rem)
  }

}
@media(max-width:700px){
  body{
    margin:1rem auto
  }
.grid,.targetLayout{
    grid-template-columns:1fr
  }

}

</style></head><body>
<h1>AnyKVM settings</h1>
<p>Use the panels below to edit your setup. The YAML file is kept compact: disabled services and default values are omitted. Save results identify changes that need a restart.</p>
<nav class="nav" aria-label="Settings sections"><button class="active" data-panel-button="general" type="button">General</button><button data-panel-button="services" type="button">Services</button><button data-panel-button="targets" type="button">Targets</button><button data-panel-button="advanced" type="button">YAML</button></nav>

<section data-panel="general"><div class="grid">
 <section class="card"><h2>Security</h2><div class="fields"><label>Admin password<input data-path="AdminPassword" data-secret type="password" autocomplete="new-password" placeholder="Unchanged"></label></div></section>
 <section class="card"><h2>TLS Certificate</h2><p class="hint">Used by HTTPS, VNC TLS, and RDP when those services are enabled.</p><div class="fields"><label>Certificate path<input data-path="TLSCert"></label><label>Private-key path<input data-path="TLSKey"></label></div></section>
 <section class="card"><h2>Behaviour</h2><div class="fields"><label>Target-switch modifier<select data-path="HotkeyMod"><option value="ctrl+alt">Ctrl + Alt</option><option value="alt+shift">Alt + Shift</option><option value="ctrl+shift">Ctrl + Shift</option><option value="ctrl+alt+shift">Ctrl + Alt + Shift</option></select></label>` + awdlSetting + `</div></section>
</div></section>

<section data-panel="services" hidden><div class="targetLayout"><aside><div id="serviceList" class="targetList"></div></aside><div id="serviceDetail">
 <section class="card" data-service="Web"><div class="serviceHead"><h2>Web</h2><label class="check"><input data-service-toggle="Web" type="checkbox"> Enabled</label></div><div class="serviceBody"><div class="grid fields"><label>HTTP<input data-path="Web.HTTP"></label><label>HTTPS<input data-path="Web.HTTPS"></label><label>Username<input data-path="Web.Username" autocomplete="username"></label><label>Password<input data-path="Web.Password" data-secret type="password" autocomplete="new-password" placeholder="Unchanged"></label><h3>Codecs</h3><div class="codecOptions"><label class="check"><input data-path="Web.Codecs.H264" type="checkbox"> H.264</label><label class="check"><input data-path="Web.Codecs.HEVC" type="checkbox"> HEVC</label><label class="check"><input data-path="Web.Codecs.AV1" type="checkbox"> AV1</label></div><label>JPEG quality<select id="webJPEGQuality"><option value="auto">Auto</option><option value="0">0</option><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5">5</option><option value="6">6</option><option value="7">7</option><option value="8">8</option><option value="9">9</option><option value="custom" disabled hidden>Custom (edit YAML)</option></select></label><label data-visible-when="Web.Codecs.H264 Web.Codecs.HEVC Web.Codecs.AV1">Video bitrate (kbps, 0 = Default)<input data-path="Web.Bitrate" type="number" min="0" max="1000000"></label><label data-visible-when="Web.Codecs.H264 Web.Codecs.HEVC Web.Codecs.AV1">Buffer frames<select id="webBufferFrames"><option value="0">Auto</option><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5">5</option><option value="6">6</option><option value="7">7</option><option value="8">8</option></select></label></div></div></section>
 <section class="card" data-service="VNC"><div class="serviceHead"><h2>VNC</h2><label class="check"><input data-service-toggle="VNC" type="checkbox"> Enabled</label></div><div class="serviceBody"><div class="grid fields"><label>Listen address<input data-path="VNC.Addr" placeholder=":5900"></label><label>Password<input data-path="VNC.Password" data-secret type="password" autocomplete="new-password" placeholder="Unchanged"></label><label class="check"><input data-path="VNC.TLS" type="checkbox"> Enable TLS</label><h3>Codecs</h3><div class="codecOptions"><label class="check"><input data-path="VNC.Codecs.JPEG" type="checkbox">JPEG</label><label class="check"><input data-path="VNC.Codecs.Tight" type="checkbox">Tight</label><label class="check"><input data-path="VNC.Codecs.H264" type="checkbox">H.264</label></div><label data-visible-when="VNC.Codecs.JPEG VNC.Codecs.Tight">JPEG quality<select id="vncJPEGQuality"><option value="auto">Auto</option><option value="0">0</option><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5">5</option><option value="6">6</option><option value="7">7</option><option value="8">8</option><option value="9">9</option><option value="custom" disabled hidden>Custom (edit YAML)</option></select></label><label data-visible-when="VNC.Codecs.H264">H.264 bitrate (kbps, 0 = Default)<input data-path="VNC.Bitrate" type="number" min="0" max="1000000"></label></div></div></section>
 <section class="card" data-service="RDP"><div class="serviceHead"><h2>RDP</h2><label class="check"><input data-service-toggle="RDP" type="checkbox"> Enabled</label></div><div class="serviceBody"><div class="grid fields"><label>Listen address<input data-path="RDP.Addr" placeholder=":3389"></label><label>Username<input data-path="RDP.Username"></label><label>Password<input data-path="RDP.Password" data-secret type="password" autocomplete="new-password" placeholder="Unchanged"></label><h3>Codecs</h3><div class="codecOptions"><label class="check"><input data-path="RDP.Codecs.ClearCodec" type="checkbox">ClearCodec</label><label class="check"><input data-path="RDP.Codecs.Planar" type="checkbox">Planar</label><label class="check"><input data-path="RDP.Codecs.Progressive" type="checkbox">Progressive</label><label class="check"><input data-path="RDP.Codecs.H264" type="checkbox">H.264</label></div><label data-visible-when="RDP.Codecs.H264">Bitrate (kbps, 0 = Auto)<input data-path="RDP.Bitrate" type="number" min="0"></label><label data-visible-when="RDP.Codecs.H264">Buffer frames<select data-path="RDP.BufferFrames" data-numeric="true"><option value="0">Auto</option><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5">5</option><option value="6">6</option><option value="7">7</option><option value="8">8</option></select></label><div class="sectionHead"><h3>Clipboard</h3><label class="check"><input data-path="RDP.Clipboard" type="checkbox">Enabled</label></div><table data-visible-when="RDP.Clipboard" class="clipboardPermissions" aria-label="Clipboard permissions"><thead><tr><th scope="col">Type</th><th scope="col">Client → Server</th><th scope="col">Server → Client</th></tr></thead><tbody><tr><th scope="row">Text</th><td><input data-path="RDP.ClipboardOptions.Text" data-bit="1" type="checkbox" aria-label="Text: Client → Server"></td><td><input data-path="RDP.ClipboardOptions.Text" data-bit="2" type="checkbox" aria-label="Text: Server → Client"></td></tr><tr><th scope="row">Rich text</th><td><input data-path="RDP.ClipboardOptions.RichText" data-bit="1" type="checkbox" aria-label="Rich text: Client → Server"></td><td><input data-path="RDP.ClipboardOptions.RichText" data-bit="2" type="checkbox" aria-label="Rich text: Server → Client"></td></tr><tr><th scope="row">Images</th><td><input data-path="RDP.ClipboardOptions.Images" data-bit="1" type="checkbox" aria-label="Images: Client → Server"></td><td><input data-path="RDP.ClipboardOptions.Images" data-bit="2" type="checkbox" aria-label="Images: Server → Client"></td></tr><tr><th scope="row">Files</th><td><input data-path="RDP.ClipboardOptions.Files" data-bit="1" type="checkbox" aria-label="Files: Client → Server"></td><td><input data-path="RDP.ClipboardOptions.Files" data-bit="2" type="checkbox" aria-label="Files: Server → Client"></td></tr></tbody></table><div class="sectionHead"><h3>Shared folders</h3><label class="check" title="Share client drives through local WebDAV using an automatic loopback port, without a password."><input data-path="RDP.Drive.Permissions" data-enable-mask="3" type="checkbox">Enabled</label></div><div data-visible-when="RDP.Drive.Permissions" class="webdavPermissions"><label class="check" title="Mount on connect and unmount on disconnect. Linux requires rclone and FUSE; headless sessions mount under /mnt."><input data-path="RDP.Drive.AutoMount" type="checkbox">Auto mount</label><label class="check" title="Allow the server to read client files."><input data-path="RDP.Drive.Permissions" data-bit="1" type="checkbox">Read</label><label class="check" title="Allow the server to create, modify, and delete client files."><input data-path="RDP.Drive.Permissions" data-bit="2" type="checkbox">Write</label></div></div></div></section>
 <section class="card" data-service="GameStream"><div class="serviceHead"><h2>GameStream</h2><label class="check"><input data-service-toggle="GameStream" type="checkbox"> Enabled</label></div><div class="serviceBody"><div class="grid fields"><label>Listen address<input data-path="GameStream.Addr" placeholder=":47989"></label><label>State file<input data-path="GameStream.State" placeholder="state.json beside executable"></label><h3>Codecs</h3><div class="codecOptions"><label class="check"><input data-path="GameStream.Codecs.HEVC" type="checkbox"> HEVC</label><label class="check"><input data-path="GameStream.Codecs.AV1" type="checkbox"> AV1</label></div><label>H.264 profile<select data-path="GameStream.Profile"><option value="constrained_baseline">Constrained baseline</option><option value="baseline">Baseline</option><option value="main">Main</option><option value="high">High</option></select></label><label class="check"><input data-path="GameStream.LowDelay" type="checkbox"> Low latency</label></div></div></section>
</div></div></section>

<section data-panel="targets" hidden><p class="hint">Choose a target to edit its capture and input devices. Add as many targets as you need; each appears on the AnyKVM home page.</p><div class="targetLayout"><aside><div id="targetList" class="targetList"></div><button id="addTarget" class="secondary" type="button">Add</button></aside><section id="targetDetail" class="card targetDetail"></section></div></section>

<section data-panel="advanced" hidden><p class="warning">For import, export, or options not represented above.</p><textarea id="config" aria-label="YAML configuration" spellcheck="false"></textarea></section>

<div class="saveRow"><button id="saveApply" type="button">Save &amp; Apply</button><button id="saveVisual" class="saveOnly" type="button">Save</button><span id="status" role="status"></span></div>
<script>
const yaml = document.getElementById('config'), status = document.getElementById('status'),
      secretMask = 'xxxxxxxx', vncQuality = document.getElementById('vncJPEGQuality'),
      webQuality = document.getElementById('webJPEGQuality'),
      webBufferFrames = document.getElementById('webBufferFrames');
let cfg, activePanel = 'general', selectedService = 'Web', selectedTarget = 0,
         devices = {video: [], audio: [], hid: []};
const serviceDefaults = {
  HTTP: ':8080',
  HTTPS: ':8443',
  VNC: ':5900',
  RDP: ':3389',
  GameStream: ':47989'
};
const get = (o, path) => path.split('.').reduce((v, k) => v && v[k], o);
const set = (o, path, value) => {
  const keys = path.split('.');
  let at = o;
  for (const key of keys.slice(0, -1)) {
    at[key] ??= {};
    at = at[key]
  }
  at[keys.at(-1)] = value
};
function statusText(text) {
  status.textContent = text
}
function renderConditionalFields() {
  document.querySelectorAll('[data-visible-when]').forEach(element => {
    const paths = element.dataset.visibleWhen;
    if (paths)
      element.hidden = !paths.split(' ').some(path => !!get(cfg, path))
  });
  document.querySelectorAll('[data-enable-mask]').forEach(input => {
    if (!input.dataset.enableMask)
      return;
    const mask = Number(get(cfg, input.dataset.path)) || 0;
    input.checked = !!mask;
    if (mask)
      input.dataset.previousMask = String(mask)
  })
}
function inputValue(input) {
  if (input.type === 'checkbox')
    return input.checked;
  if (input.type === 'number' || input.dataset.numeric === 'true')
    return input.value === '' ? 0 : Number(input.value);
  return input.value
}
function vncQualityFromLevel(level) {
  return 10 + Math.floor(level * 85 / 9)
}
function vncLevelForQuality(quality) {
  for (let level = 0; level <= 9; level++)
    if (vncQualityFromLevel(level) === quality)
      return String(level);
  return 'custom'
}
function fillFields() {
  document.querySelectorAll('[data-path]').forEach(input => {
    const value = get(cfg, input.dataset.path);
    if (input.type === 'checkbox')
      input.checked = input.dataset.bit ? !!(Number(value) & Number(input.dataset.bit)) : !!value;
    else if (input.dataset.secret) {
      input.value = value ? secretMask : '';
      input.dataset.secretOriginal = value || '';
      input.dataset.masked = value ? 'true' : ''
    } else
      input.value = value ?? ''
  });
  const quality = Number(cfg.VNC.JPEGQuality) || 0;
  vncQuality.value = quality === 0 ? 'auto' : vncLevelForQuality(quality);
  const webJPEG = Number(cfg.Web.JPEGQuality) || 0;
  webQuality.value = webJPEG === 0 ? 'auto' : vncLevelForQuality(webJPEG);
  const frames = cfg.Web.BufferFrames;
  webBufferFrames.value = frames >= 0 && frames <= 8 ? String(frames) : '0';
  renderConditionalFields()
}
function bindFields() {
  document.querySelectorAll('[data-path]').forEach(input => {
    if (input.dataset.secret)
      input.onfocus = () => {
        if (input.dataset.masked === 'true') {
          input.value = '';
          input.dataset.masked = ''
        }
      };
    const update = () => {
      if (input.dataset.secret && !input.value) {
        set(cfg, input.dataset.path, input.dataset.secretOriginal);
        return
      }
      if (input.dataset.enableMask) {
        const mask = Number(get(cfg, input.dataset.path)) || 0;
        if (!input.checked && mask)
          input.dataset.previousMask = String(mask);
        set(cfg, input.dataset.path,
            input.checked ? Number(input.dataset.previousMask || input.dataset.enableMask) : 0);
        fillFields()
      } else if (input.dataset.bit) {
        const bit = Number(input.dataset.bit), value = Number(get(cfg, input.dataset.path)) || 0;
        set(cfg, input.dataset.path, input.checked ? value | bit : value & ~bit)
      } else
        set(cfg, input.dataset.path, inputValue(input));
      renderConditionalFields()
    };
    input.addEventListener('input', update);
    input.addEventListener('change', update)
  })
}
function renderServices() {
  const list = document.getElementById("serviceList");
  list.replaceChildren();
  document.querySelectorAll("[data-service]").forEach(card => {
    const service = card.dataset.service,
          enabled =
              service === "Web" ? !!(cfg.Web.HTTP || cfg.Web.HTTPS) : !!get(cfg, service + ".Addr");
    card.hidden = service !== selectedService;
    card.querySelector("[data-service-toggle]").checked = enabled;
    card.querySelector(".serviceBody").hidden = !enabled;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "targetCard" + (service === selectedService ? " active" : "");
    button.textContent = service;
    button.onclick = () => {
      selectedService = service;
      renderServices()
    };
    list.append(button)
  })
}
function targetInput(target, path, label, type = 'text', deviceKind = '', placeholder = '') {
  const wrap = document.createElement('label');
  wrap.textContent = label;
  const input = document.createElement('input');
  input.type = type;
  input.placeholder = placeholder;
  if (path === 'HID.WheelScale')
    input.step = 'any';
  if (deviceKind) {
    const listID = 'device-' + deviceKind;
    input.setAttribute('list', listID);
    let list = document.getElementById(listID);
    if (!list) {
      list = document.createElement('datalist');
      list.id = listID;
      document.body.append(list)
    }
    list.replaceChildren(...(deviceKind === 'audio' || deviceKind === 'hid' ?
                                 [
                                   {value: 'auto', label: 'Auto'},
                                   ...devices[deviceKind].filter(device => device.value !== 'auto')
                                 ] :
                                 devices[deviceKind])
                             .map(device => {
                               const option = document.createElement('option');
                               option.value = device.value;
                               option.label = device.label;
                               return option
                             }))
  }
  input.value = get(target, path) ?? '';
  input.addEventListener('input', () => set(target, path, inputValue(input)));
  input.addEventListener('change', () => {
    set(target, path, inputValue(input));
    if (path === 'Video.Source')
      renderTargets()
  });
  wrap.append(input);
  return wrap
}
function targetSelect(target, path, label, options) {
  const wrap = document.createElement('label');
  wrap.textContent = label;
  const select = document.createElement('select');
  for (const [value, text] of options) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = text;
    select.append(option)
  }
  select.value = get(target, path) || (path === 'Video.CursorRendering' ? 'client' : options[0][0]);
  select.onchange = () => set(target, path, select.value);
  wrap.append(select);
  return wrap
}
function targetCheck(target, path, label) {
  const wrap = document.createElement('label');
  wrap.className = 'check';
  const input = document.createElement('input');
  input.type = 'checkbox';
  input.checked = !!get(target, path);
  input.onchange = () => set(target, path, input.checked);
  wrap.append(input, document.createTextNode(label));
  return wrap
}
function targetGroup(name, ...controls) {
  const group = document.createElement('section');
  group.className = 'targetGroup';
  const heading = document.createElement('h3');
  heading.textContent = name;
  const fields = document.createElement('div');
  fields.className = 'grid fields';
  fields.append(...controls);
  group.append(heading, fields);
  return group
}
function targetWheelScale(target) {
  const wrap = document.createElement('label'), heading = document.createElement('span'),
        title = document.createElement('span'), value = document.createElement('output'),
        input = document.createElement('input');
  wrap.className = 'wheelScale';
  heading.className = 'rangeHeading';
  title.textContent = 'Wheel scale';
  input.type = 'range';
  input.min = '-1';
  input.max = '1';
  input.step = '0.01';
  input.value = String(get(target, 'HID.WheelScale') ?? 1);
  value.textContent = input.value;
  const paint = () => {
    const position = (Math.max(-1, Math.min(1, Number(input.value))) + 1) * 50,
          start = Math.min(50, position), end = Math.max(50, position);
    input.style.setProperty(
        '--wheel-track',
        'linear-gradient(to right,#44515e ' + start + '%,#4d8df7 ' + start + '%,#4d8df7 ' + end +
            '%,#44515e ' + end + '%)')
  };
  paint();
  input.oninput = () => {
    const scale = Number(input.value);
    set(target, 'HID.WheelScale', scale);
    value.textContent = String(scale);
    paint()
  };
  wrap.append(title, input, value);
  return wrap
}
function targetMouseGroup(target) {
  const group = document.createElement('section');
  group.className = 'targetMouseGroup';
  const heading = document.createElement('h4');
  heading.textContent = 'Mouse';
  const fields = document.createElement('div');
  fields.className = 'grid fields';
  fields.append(targetChoiceMatrix(target), targetWheelScale(target));
  group.append(heading, fields);
  return group
}
function targetChoiceMatrix(target) {
  const wrap = document.createElement('div');
  wrap.className = 'targetChoices';
  for (const [path, title, options, fallback] of [
           [
             'HID.MouseMode', 'Mode', [['absolute', 'Absolute'], ['relative', 'Relative']],
             'absolute'
           ],
           [
             'Video.CursorRendering', 'Cursor', [['client', 'Client'], ['remote', 'Remote']],
             'client'
           ]]) {
    const group = document.createElement('div'), heading = document.createElement('span'),
          choices = document.createElement('div');
    group.className = 'targetChoiceRow';
    group.setAttribute('role', 'radiogroup');
    group.setAttribute('aria-label', title);
    heading.textContent = title;
    choices.className = 'radioOptions';
    for (const [value, text] of options) {
      const label = document.createElement('label'), input = document.createElement('input');
      label.className = 'check';
      input.type = 'radio';
      input.name = 'target-' + path;
      input.value = value;
      input.checked = (get(target, path) || fallback) === value;
      input.setAttribute('aria-label', title + ': ' + text);
      input.onchange = () => {
        if (input.checked)
          set(target, path, value)
      };
      label.append(input, document.createTextNode(text));
      choices.append(label)
    }
    group.append(heading, choices);
    wrap.append(group)
  }
  return wrap
}
const modeRequests = new Map();
function formatFPS(value) {
  return String(Math.round(value * 1000) / 1000)
}
function supportedModeOptions(modes) {
  const options = new Map();
  for (const mode of modes) {
    const rates = new Set([
      mode.minFPS, mode.maxFPS,
      ...[15, 24, 25, 30, 48, 50, 60, 90, 120, 144, 240].filter(
          rate => rate >= mode.minFPS && rate <= mode.maxFPS)
    ]);
    for (const rate of rates) {
      if (!(rate > 0))
        continue;
      const value = mode.width + 'x' + mode.height + '@' + formatFPS(rate) + ':' + mode.pixelFormat;
      options.set(value, {
        value,
        label: mode.width + ' × ' + mode.height + ' · ' + formatFPS(rate) + ' FPS · ' +
            mode.pixelFormat.toUpperCase()
      });
    }
  }
  return [...options.values()];
}
async function getVideoModes(device, refresh = false) {
  if (refresh)
    modeRequests.delete(device);
  if (!modeRequests.has(device)) {
    const request = fetch('api/video-modes?device=' + encodeURIComponent(device), {
                      cache: 'no-store',
                      signal: AbortSignal.timeout(10000)
                    }).then(async response => {
      if (!response.ok)
        throw new Error(await response.text());
      return (await response.json()).modes || []
    });
    modeRequests.set(device, request);
    request.catch(() => {
      if (modeRequests.get(device) === request)
        modeRequests.delete(device)
    });
  }
  return modeRequests.get(device);
}
function targetModeEditor(target) {
  const wrap = targetInput(target, 'Video.Mode', 'Mode');
  const input = wrap.querySelector('input');
  input.placeholder = 'Auto';
  input.title = 'Leave empty for the source default, or enter a mode such as 1920x1080@60:nv12.';
  const list = document.createElement('datalist');
  list.id = 'capture-modes';
  input.setAttribute('list', list.id);
  wrap.append(list);
  const device = get(target, 'Video.Source') || '';
  if (!device || device === 'screen' || device.startsWith('screen:') ||
      /^(vnc|ard|airplay|android|scrcpy|ios-usb|coredevice):/i.test(device))
    return wrap;
  let revision = 0;
  async function discover() {
    const requestRevision = ++revision;
    try {
      const modes = await getVideoModes(device);
      if (requestRevision !== revision || !wrap.isConnected ||
          get(target, 'Video.Source') !== device)
        return;
      list.replaceChildren(...supportedModeOptions(modes).map(entry => {
        const option = document.createElement('option');
        option.value = entry.value;
        option.label = entry.label;
        return option;
      }));
    } catch (error) {
      if (requestRevision === revision && wrap.isConnected)
        input.title =
            'Mode discovery unavailable. You can enter a capture mode manually, or focus this field to retry.';
    }
  }
  input.addEventListener('focus', () => {void discover()});
  queueMicrotask(() => {void discover()});
  return wrap;
}
function renderTargets() {
  const list = document.getElementById('targetList'),
        detail = document.getElementById('targetDetail');
  if (selectedTarget >= cfg.Targets.length)
    selectedTarget = cfg.Targets.length - 1;
  list.replaceChildren();
  cfg.Targets.forEach((target, index) => {
    const card = document.createElement('button');
    card.type = 'button';
    card.className = 'targetCard' + (index === selectedTarget ? ' active' : '');
    const title = document.createElement('strong');
    title.textContent = target.Name || 'Unnamed target';
    card.append(title);
    card.onclick = () => {
      selectedTarget = index;
      renderTargets()
    };
    list.append(card)
  });
  detail.replaceChildren();
  const target = cfg.Targets[selectedTarget];
  if (!target)
    return;
  const head = document.createElement('div');
  head.className = 'targetHead';
  const title = document.createElement('h2');
  title.textContent = 'Target details';
  head.append(title);
  detail.append(head);
  const fields = document.createElement('div');
  fields.className = 'fields';
  fields.append(targetInput(target, 'Name', 'Name'));
  detail.append(fields);
  detail.append(
      targetGroup(
          'Video', targetInput(target, 'Video.Source', 'Source', 'text', 'video'),
          targetModeEditor(target)),
      targetGroup('Audio', targetInput(target, 'Audio.Source', 'Source', 'text', 'audio')),
      targetGroup(
          'HID', targetInput(target, 'HID.Device', 'Device', 'text', 'hid'),
          targetMouseGroup(target)));
  if (/^vnc:\/\//.test(get(target, 'Video.Source') || '')) {
    const mac = targetInput(target, 'WoL.MAC', 'MAC');
    mac.title = 'Leave empty to disable Wake-on-LAN.';
    const broadcast = targetInput(target, 'WoL.Broadcast', 'IP', 'text', '', '255.255.255.255');
    broadcast.title =
        'IP address or hostname; UDP port defaults to 9. Examples: 192.168.1.255, ::1, or [::1]:7. Empty uses 255.255.255.255:9.';
    const wake = targetGroup('Wake-on-LAN', mac, broadcast);
    wake.title =
        'If the target is unreachable, send a magic packet and retry VNC for up to 1 second.';
    detail.append(wake)
  }
  const actions = document.createElement('div');
  actions.className = 'detailActions';
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'danger';
  remove.textContent = 'Remove';
  remove.onclick = () => {
    if (cfg.Targets.length === 1) {
      statusText('At least one target is required.');
      return
    }
    cfg.Targets.splice(selectedTarget, 1);
    selectedTarget = Math.max(0, selectedTarget - 1);
    renderTargets()
  };
  actions.append(remove);
  detail.append(actions)
}
function showPanel(name) {
  activePanel = name;
  document.querySelectorAll('[data-panel]')
      .forEach(panel => panel.hidden = panel.dataset.panel !== name);
  document.querySelectorAll('[data-panel-button]')
      .forEach(button => button.classList.toggle('active', button.dataset.panelButton === name))
}
async function loadDevices() {
  try {
    const response =
        await fetch('api/devices', {cache: 'no-store', signal: AbortSignal.timeout(10000)});
    if (!response.ok)
      throw new Error(await response.text());
    const discovered = await response.json();
    devices = {
      video: discovered.video || [],
      audio: discovered.audio || [],
      hid: discovered.hid || []
    };
    renderTargets()
  } catch (error) {
    console.warn('Device discovery unavailable:', error)
  }
}
async function load() {
  const [jsonResponse, yamlResponse] = await Promise.all(
      [fetch('api/config.json', {cache: 'no-store'}), fetch('api/config', {cache: 'no-store'})]);
  if (!jsonResponse.ok)
    throw new Error(await jsonResponse.text());
  if (!yamlResponse.ok)
    throw new Error(await yamlResponse.text());
  cfg = await jsonResponse.json();
  yaml.value = (await yamlResponse.json()).yaml;
  fillFields();
  renderServices();
  renderTargets()
}
function saveCurrent(button, apply) {
  return activePanel === 'advanced' ? save('api/config', {yaml: yaml.value}, button, apply) :
                                      save('api/config.json', cfg, button, apply)
}
async function save(url, body, button, apply) {
  button.disabled = true;
  statusText('Saving…');
  try {
    const response = await fetch(
        url + (apply ? '?apply=1' : ''),
        {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
    if (!response.ok)
      throw new Error(await response.text());
    const result = await response.json();
    const messages = [];
    if (apply && result.applied?.length)
      messages.push('Applied now: ' + result.applied.join(', ') + '.');
    if (apply && result.restartRequired)
      messages.push('Restart required for: ' + result.restartReasons.join(', ') + '.');
    if (!apply)
      messages.push('Saved without applying.');
    if (!messages.length)
      messages.push('Saved and applied.');
    if (result.settingsAuthenticationChanged) {
      messages.push('Sign in again to continue editing settings.');
      statusText(messages.join(' '));
      return
    }
    await load();
    statusText(messages.join(' '))
  } catch (error) {
    statusText(error.message)
  } finally {
    button.disabled = false
  }
}
bindFields();
vncQuality.onchange = () => {
  cfg.VNC.JPEGQuality =
      vncQuality.value === 'auto' ? 0 : vncQualityFromLevel(Number(vncQuality.value))
};
webQuality.onchange = () => {
  cfg.Web.JPEGQuality =
      webQuality.value === 'auto' ? 0 : vncQualityFromLevel(Number(webQuality.value))
};
webBufferFrames.onchange = () => {
  cfg.Web.BufferFrames = Number(webBufferFrames.value)
};
document.querySelectorAll('[data-service-toggle]').forEach(toggle => toggle.onchange = () => {
  const service = toggle.dataset.serviceToggle;
  if (service === 'Web') {
    if (toggle.checked) {
      if (!cfg.Web.HTTP && !cfg.Web.HTTPS) {
        cfg.Web.HTTP = serviceDefaults.HTTP;
        cfg.Web.HTTPS = serviceDefaults.HTTPS
      }
    } else {
      cfg.Web.HTTP = '';
      cfg.Web.HTTPS = ''
    }
  } else if (toggle.checked) {
    if (!get(cfg, service + '.Addr'))
      set(cfg, service + '.Addr', serviceDefaults[service])
  } else
    set(cfg, service + '.Addr', '');
  fillFields();
  renderServices()
});
document.querySelectorAll('[data-panel-button]')
    .forEach(button => button.onclick = () => showPanel(button.dataset.panelButton));
document.getElementById('addTarget').onclick = () => {
  cfg.Targets.push({
    Name: 'New target',
    Video: {Source: 'screen', Mode: '', CursorRendering: 'client'},
    Audio: {Source: 'auto'},
    HID: {Device: 'auto', MouseMode: 'absolute', WheelScale: 1}
  });
  selectedTarget = cfg.Targets.length - 1;
  renderTargets()
};
document.getElementById('saveApply').onclick = () =>
    saveCurrent(document.getElementById('saveApply'), true);
document.getElementById('saveVisual').onclick = () =>
    saveCurrent(document.getElementById('saveVisual'), false);
load().then(() => loadDevices()).catch(error => statusText(error.message));
</script></body></html>`
