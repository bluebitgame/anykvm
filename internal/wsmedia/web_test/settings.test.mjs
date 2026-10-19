import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

test('wheel scale slider ranges from -1 to 1 and updates the displayed value and config', () => {
  const source=readFileSync(new URL('../settings_page.go',import.meta.url),'utf8');
  const script=source.split('<script>')[1].split('\nbindFields();')[0];
  const element=()=>({children:[],style:{setProperty(k,v){this[k]=v}},append(...nodes){this.children.push(...nodes)}});
  const target={HID:{WheelScale:0.5}};
  const wrap=runInNewContext(script+'\ntargetWheelScale(target)',{target,document:{getElementById:()=>({}),createElement:element}});
  const input=wrap.children[1],output=wrap.children[2];
  assert.equal(wrap.children[0].textContent,'Wheel scale');
  assert.equal(input.type,'range');
  assert.equal(wrap.className,'wheelScale');
  assert.equal(input.min,'-1');assert.equal(input.max,'1');assert.equal(input.step,'0.01');
  assert.equal(output.textContent,'0.5');
  for(const value of ['-1','-0.23','0','0.37','1']){input.value=value;input.oninput();assert.equal(target.HID.WheelScale,Number(value));assert.equal(output.textContent,value);const position=(Number(value)+1)*50,start=Math.min(50,position),end=Math.max(50,position);assert.equal(input.style['--wheel-track'],'linear-gradient(to right,#44515e '+start+'%,#4d8df7 '+start+'%,#4d8df7 '+end+'%,#44515e '+end+'%)')}
});

test('Mouse mode and Cursor use separate radio groups and preserve configuration values', () => {
  const source=readFileSync(new URL('../settings_page.go',import.meta.url),'utf8');
  const script=source.split('<script>')[1].split('\nbindFields();')[0];
  const element=()=>({children:[],append(...nodes){this.children.push(...nodes)},setAttribute(k,v){this[k]=v}});
  for(const defaults of [true,false]){
    const target=defaults?{}:{HID:{MouseMode:'relative'},Video:{CursorRendering:'remote'}};
    const table=runInNewContext(script+'\ntargetChoiceMatrix(target)',{target,document:{getElementById:()=>({}),createElement:element,createTextNode:text=>({textContent:text})}});
    assert.equal(table.className,'targetChoices');
    const rows=table.children;
    assert.deepEqual(rows.map(r=>r.children[0].textContent),['Mode','Cursor']);
    const groups=rows.map(row=>row.children[1].children.map(label=>label.children[0]));
    assert.ok(groups.flat().every(input=>input.type==='radio'));
    assert.ok(groups.every(inputs=>inputs[0].name===inputs[1].name));
    assert.notEqual(groups[0][0].name,groups[1][0].name);
    assert.deepEqual(groups.map(inputs=>inputs.map(input=>input.checked)),defaults?[[true,false],[true,false]]:[[false,true],[false,true]]);
    groups[0][1].checked=true;groups[0][1].onchange();
    assert.equal(target.HID.MouseMode,'relative');
    groups[1][0].checked=true;groups[1][0].onchange();
    assert.equal(target.Video.CursorRendering,'client');
    assert.equal(target.HID.MouseMode,'relative');
  }
});

test('Web codec fields and JPEG level labels match enabled codecs', () => {
  const source=readFileSync(new URL('../settings_page.go',import.meta.url),'utf8');
  const script=source.split('<script>')[1].split('\nbindFields();')[0];
  const fields=[...source.matchAll(/data-visible-when="(Web\.[^"]+)"/g)].map(m=>({dataset:{visibleWhen:m[1]}}));
  assert.equal(fields.length,2);
  const config={Web:{Codecs:{H264:false,HEVC:false,AV1:false}}};
  const context={config,document:{getElementById:()=>({}),querySelectorAll:()=>fields}};
  runInNewContext(script+'\ncfg=config;renderConditionalFields()',context);
  assert.ok(fields.every(f=>f.hidden));
  config.Web.Codecs.HEVC=true;
  runInNewContext('renderConditionalFields()',context);
  assert.equal(fields[0].hidden,false);
  assert.equal(fields[1].hidden,false);
  config.Web.Codecs.HEVC=false;
  config.Web.Codecs.AV1=true;
  runInNewContext('renderConditionalFields()',context);
  assert.ok(fields.every(f=>!f.hidden));
  config.Web.Codecs.AV1=false;
  config.Web.Codecs.H264=true;
  runInNewContext('renderConditionalFields()',context);
  assert.ok(fields.every(f=>!f.hidden));
  const quality=source.split('id="vncJPEGQuality"')[1].split('</select>')[0];
  assert.deepEqual([...quality.matchAll(/<option value="([0-9])">([^<]+)<\/option>/g)].map(m=>m[2]),['0','1','2','3','4','5','6','7','8','9']);
  assert.equal(runInNewContext('vncQualityFromLevel(9)',context),95);
});

test('WebDAV uses permission masks and auto mount without an address field', () => {
  const source=readFileSync(new URL('../settings_page.go',import.meta.url),'utf8');
  const script=source.split('<script>')[1].split('\nbindFields();')[0];
  assert.ok(!source.includes('RDP.Drive.Addr'));
  assert.ok(source.includes('data-path="RDP.Drive.AutoMount"'));
  const toggle={type:'checkbox',dataset:{path:'RDP.Drive.Permissions',enableMask:'3'},handlers:{},addEventListener(k,v){this.handlers[k]=v}};
  const permissions=[1,2].map(bit=>({type:'checkbox',dataset:{path:'RDP.Drive.Permissions',bit:String(bit)},handlers:{},addEventListener(k,v){this.handlers[k]=v}}));
  const autoMount={type:'checkbox',dataset:{path:'RDP.Drive.AutoMount'},handlers:{},addEventListener(k,v){this.handlers[k]=v}};
  const inputs=[toggle,...permissions,autoMount];
  const config={VNC:{},Web:{},RDP:{Drive:{Permissions:1,AutoMount:true}}};
  const context={config,document:{getElementById:()=>({}),querySelectorAll:selector=>selector==='[data-path]'?inputs:selector==='[data-enable-mask]'?[toggle]:[]}};
  runInNewContext(script+'\ncfg=config;fillFields();bindFields();',context);
  assert.equal(toggle.checked,true);
  toggle.checked=false;toggle.handlers.change();
  assert.equal(config.RDP.Drive.Permissions,0);
  assert.equal(config.RDP.Drive.AutoMount,true);
  toggle.checked=true;toggle.handlers.change();
  assert.equal(config.RDP.Drive.Permissions,1);
  for(let mask=0;mask<4;mask++){
    config.RDP.Drive.Permissions=mask;
    runInNewContext('fillFields()',context);
    assert.deepEqual(permissions.map(p=>p.checked),[!!(mask&1),!!(mask&2)]);
    permissions[0].checked=!permissions[0].checked;permissions[0].handlers.change();
    assert.equal(config.RDP.Drive.Permissions,mask^1);
    assert.equal(toggle.checked,!!(mask^1));
  }
  autoMount.checked=false;autoMount.handlers.change();
  assert.equal(config.RDP.Drive.AutoMount,false);
  delete toggle.dataset.previousMask;
  config.RDP.Drive.Permissions=0;toggle.checked=true;toggle.handlers.change();
  assert.equal(config.RDP.Drive.Permissions,3);
});

test('codec-specific fields follow selections without clearing values', () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const fields = [...source.matchAll(/data-visible-when="([^"]+)"/g)].filter(match=>!match[1].startsWith('Web.')).map(match => ({dataset:{visibleWhen:match[1]}}));
  assert.equal(fields.length, 6);
  const toggle = {type:'checkbox', dataset:{path:'VNC.Codecs.H264'}, handlers:{}, addEventListener(name, fn){this.handlers[name]=fn}};
  const config = { VNC:{Codecs:{JPEG:false,Tight:false,H264:false},Bitrate:12000,JPEGQuality:75}, RDP:{Clipboard:false,Codecs:{H264:false},Bitrate:12000,BufferFrames:2},Web:{} };
  const context = {config, document:{getElementById:()=>({}),querySelectorAll:selector=>selector==='[data-visible-when]'?fields:[toggle]}};
  runInNewContext(script+'\ncfg=config;fillFields();bindFields();',context);
  assert.ok(fields.every(field=>field.hidden));
  toggle.checked=true;
  toggle.handlers.change();
  assert.equal(fields.find(field=>field.dataset.visibleWhen==='VNC.Codecs.H264').hidden,false);
  assert.equal(fields.find(field=>field.dataset.visibleWhen==='VNC.Codecs.JPEG VNC.Codecs.Tight').hidden,true);
  assert.ok(source.indexOf('id="vncJPEGQuality"')<source.indexOf('data-path="VNC.Bitrate"'));
  config.VNC.Codecs.Tight=true;
  config.RDP.Codecs.H264=true;
  config.RDP.Clipboard=true;
  config.RDP.Drive={Permissions:3};
  runInNewContext('renderConditionalFields()',context);
  assert.ok(fields.every(field=>!field.hidden));
  config.VNC.Codecs.H264=false;
  config.VNC.Codecs.Tight=false;
  config.RDP.Codecs.H264=false;
  config.RDP.Clipboard=false;
  config.RDP.Drive.Permissions=0;
  runInNewContext('fillFields()',context);
  assert.ok(fields.every(field=>field.hidden));
  assert.equal(config.VNC.Bitrate,12000);
  assert.equal(config.VNC.JPEGQuality,75);
  assert.equal(config.RDP.BufferFrames,2);
});

test('clipboard direction checkboxes round-trip all permission masks independently', () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  assert.equal((source.match(/data-bit="[12]"/g) || []).length, 10);
  for (const type of ['Text', 'RichText', 'Images', 'Files']) {
    for (let mask = 0; mask < 4; mask++) {
      const inputs = [1, 2].map(bit => ({
        type: 'checkbox', dataset: { path: 'RDP.ClipboardOptions.' + type, bit: String(bit) },
        handlers: {}, addEventListener(name, handler) { this.handlers[name] = handler; },
      }));
      const cfg = { VNC: {}, Web: {}, RDP: { ClipboardOptions: { [type]: mask } } };
      const context = {
        config: cfg,
        document: { getElementById: () => ({}), querySelectorAll: () => inputs },
      };
      runInNewContext(script + '\ncfg=config;fillFields();bindFields();', context);
      assert.deepEqual(inputs.map(input => input.checked), [!!(mask & 1), !!(mask & 2)]);
      let expected = mask;
      for (const input of inputs) {
        input.checked = !input.checked;
        input.handlers.change();
        expected ^= Number(input.dataset.bit);
        assert.equal(cfg.RDP.ClipboardOptions[type], expected);
      }
    }
  }
});

test('settings render while device discovery is pending or fails', async () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const rendered = [];
  let rejectDiscovery;
  const discovery = new Promise((_, reject) => { rejectDiscovery = reject; });
  const config = { Web: { HTTP: '127.0.0.1:8080' } };
  const context = {
    document: { getElementById: () => ({}) },
    AbortSignal,
    console: { warn() {} },
    rendered,
    fetch: async url => {
      if (url === 'api/devices') return discovery;
      return { ok: true, json: async () => url.endsWith('.json') ? config : { yaml: 'web: {}' } };
    },
  };
  await runInNewContext(script + `
    fillFields=()=>rendered.push(cfg.Web.HTTP);
    renderServices=()=>rendered.push('services');
    renderTargets=()=>rendered.push('targets');
    load().then(()=>{void loadDevices()});
  `, context);
  assert.deepEqual(rendered, ['127.0.0.1:8080', 'services', 'targets']);
  rejectDiscovery(new Error('discovery failed'));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(rendered.length, 3);
});

test('capture modes retain device format and frame-rate constraints', () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const values = runInNewContext(script + `
    supportedModeOptions([
      {width:1920,height:1080,pixelFormat:'nv12',minFPS:30,maxFPS:60},
      {width:2560,height:1440,pixelFormat:'yuyv422',minFPS:30,maxFPS:30},
      {width:1280,height:720,pixelFormat:'nv12',minFPS:59.94,maxFPS:59.94},
    ]).map(option=>option.value)
  `, { document: { getElementById: () => ({}) } });
  assert.ok(values.includes('1920x1080@60:nv12'));
  assert.ok(values.includes('1920x1080@30:nv12'));
  assert.ok(values.includes('2560x1440@30:yuyv422'));
  assert.ok(values.includes('1280x720@59.94:nv12'));
  assert.ok(!values.includes('2560x1440@60:yuyv422'));
  assert.ok(!values.includes('1920x1080@60:yuyv422'));
});

test('capture mode discovery escapes device names and can retry after failure', async () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const calls = [];
  let failed = true;
  const context = {
    document: { getElementById: () => ({}) }, AbortSignal,
    fetch: async url => {
      calls.push(url);
      if (failed) { failed = false; throw new Error('temporary failure'); }
      return { ok: true, json: async () => ({ modes: [{ width:1920,height:1080,pixelFormat:'nv12',minFPS:60,maxFPS:60 }] }) };
    },
  };
  await runInNewContext(script, context);
  await assert.rejects(runInNewContext("getVideoModes('Capture & USB')", context));
  const modes = await runInNewContext("getVideoModes('Capture & USB')", context);
  assert.equal(modes[0].pixelFormat, 'nv12');
  await runInNewContext("getVideoModes('Capture & USB')", context);
  assert.equal(calls.length, 2);
  assert.equal(calls[1], 'api/video-modes?device=Capture%20%26%20USB');
});


test('shared save buttons submit the active editor content', () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const calls = [];
  const yamlInput = { value: 'targets: []' };
  const button = {};
  runInNewContext(script + `
    cfg={Targets:[{Name:'visual'}]};
    save=(url,body,button,apply)=>calls.push({url,body,button,apply});
    saveCurrent(button,true);
    showPanel('advanced');
    saveCurrent(button,false);
    saveCurrent(button,true);
    showPanel('targets');
    saveCurrent(button,false);
  `, {
    calls, button,
    document: {
      getElementById: id => id === 'config' ? yamlInput : {},
      querySelectorAll: () => [],
    },
  });
  assert.deepEqual(calls.map(c => c.url), ['api/config.json','api/config','api/config','api/config.json']);
  assert.deepEqual(calls.map(c => c.apply), [true,false,true,false]);
  assert.equal(calls[1].body.yaml, 'targets: []');
  assert.equal(calls[2].body.yaml, 'targets: []');
  assert.equal(calls[3].body.Targets[0].Name, 'visual');
  assert.ok(calls.every(c => c.button === button));
});


test('Windows device names with colons still discover capture modes', () => {
  const source = readFileSync(new URL('../settings_page.go', import.meta.url), 'utf8');
  const script = source.split('<script>')[1].split('\nbindFields();')[0];
  const queued=[];
  const wrap={append(){},querySelector(){return {setAttribute(){},addEventListener(){}}}};
  runInNewContext(script+`
    targetInput=()=>wrap;
    targetModeEditor({Video:{Source:'USB Capture: HDMI'}});
    targetModeEditor({Video:{Source:'screen:0'}});
    targetModeEditor({Video:{Source:'vnc:host:5900'}});
  `, {
    wrap,
    document:{getElementById:()=>({}),createElement:()=>({})},
    queueMicrotask:callback=>queued.push(callback),
  });
  assert.equal(queued.length,1);
});

test('WoL editor is available only for VNC targets and preserves hidden values', () => {
  const source=readFileSync(new URL('../settings_page.go',import.meta.url),'utf8');
  const script=source.split('<script>')[1].split('\nbindFields();')[0];
  const element=()=>({children:[],append(...nodes){this.children.push(...nodes)},replaceChildren(...nodes){this.children=nodes}});
  const sections=[];
  const paths=[];
  const target={Name:'Remote',Video:{Source:'vnc://host'},WoL:{MAC:'02:11:22:33:44:55'}};
  const context={config:{Targets:[target]},paths,element,document:{getElementById:element,createElement:tag=>{const node=element();if(tag==='section')sections.push(node);return node}}};
  runInNewContext(script+`;cfg=config;targetInput=(target,path,label,type,kind,placeholder)=>{paths.push(path);return Object.assign(element(),{path,label,placeholder})};targetSelect=element;targetChoiceMatrix=element;targetWheelScale=element;targetModeEditor=element;renderTargets();`,context);
  assert.ok(paths.includes('WoL.MAC'));
  assert.ok(paths.includes('WoL.Broadcast'));
  assert.ok(!paths.includes('WoL.WaitSeconds'));
  const wake=sections.find(section=>section.children[0]?.textContent==='Wake-on-LAN');
  assert.equal(wake.className,'targetGroup');
  assert.equal(wake.children[1].className,'grid fields');
  assert.deepEqual(wake.children[1].children.map(field=>field.label),['MAC','IP']);
  assert.equal(wake.children[1].children[1].placeholder,'255.255.255.255');
  assert.equal(target.WoL.Broadcast,undefined);
  target.Video.Source='screen';paths.length=0;
  runInNewContext('renderTargets()',context);
  assert.ok(!paths.some(path=>path.startsWith('WoL.')));
  assert.equal(target.WoL.MAC,'02:11:22:33:44:55');
});
