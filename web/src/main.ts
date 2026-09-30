import './style.css';
import * as tus from 'tus-js-client';

type Session = {username:string;csrf:string};
type Entry = {id:string;name:string;size:number;created_at:string};
type Capacity = {total:number;used:number;available:number;reserve:number;upload_available:number;files_bytes:number;partial_bytes:number;pending_bytes:number};
type Share = {id:string;file_id:string;file_name:string;size:number;created_at:string;expires_at:string|null;url:string;download_url:string};
type RemoteUpload = Entry & {offset:number;upload_url:string};
type Job = {key:string;name:string;size:number;offset:number;state:string;file?:File;upload?:tus.Upload;url?:string;error?:string;speed:number;at:number;last:number;generation?:number};
const root=document.querySelector<HTMLDivElement>('#app')!;
let session:Session|null=null;
let files:Entry[]=[]; let shares:Share[]=[]; let capacity:Capacity|null=null;
let jobs:Job[]=[];let tab='files';let search='';let sort='new';let refreshTimer:number|undefined;
let refreshBusy=false;let renderPending=false;
let listSnapshot='';let capacitySnapshot='';let capacityExpanded=false;let loaded=false;
const chunkSize=8*1024*1024;
const labels:Record<string,string>={queued:'排队中',uploading:'上传中',paused:'已暂停',waiting:'等待选择原文件',done:'已完成',error:'需重试'};

function el<K extends keyof HTMLElementTagNameMap>(tag:K, cls='',text=''):HTMLElementTagNameMap[K]{
 const n=document.createElement(tag);n.className=cls;if(text)n.textContent=text;return n;
}
function action(text:string,fn:()=>unknown,cls='button secondary'){
 const b=el('button',cls,text);b.type='button';
 b.addEventListener('click',async()=>{
  const trigger=b.closest('details')?.querySelector<HTMLElement>('summary')||b;
  const fileID=b.closest<HTMLElement>('[data-file]')?.dataset.file;
  b.disabled=true;
  try{await fn()}catch(e){message(errorText(e),true)}finally{
   b.disabled=false;
   if(!document.querySelector('dialog[open]')&&(document.activeElement===document.body||document.activeElement===b)){
    const fallback=fileID?document.querySelector<HTMLElement>('[data-file="'+fileID+'"] summary'):null;
    (trigger.isConnected?trigger:fallback||document.querySelector<HTMLElement>('#main-content'))?.focus({preventScroll:true});
   }
  }
 });
 return b;
}
function icon(name:string){
 const paths:Record<string,string>={files:'<path d="M5 4h14v16H5zM8 8h8M8 12h8M8 16h5"/>',upload:'<path d="m7 8 5-5 5 5M12 3v12M5 15v5h14v-5"/>',share:'<path d="M14 3h7v7M21 3 10 14M10 5H4v15h15v-6"/>',refresh:'<path d="M20 7v5h-5M4 17v-5h5M6.3 7a7 7 0 0 1 11.9-.7L20 9M4 15l1.8 2.7A7 7 0 0 0 17.7 17"/>',more:'<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',close:'<path d="m6 6 12 12M6 18 18 6"/>'};
 return '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'+(paths[name]||paths.files)+'</svg>';
}
function switchTab(next:string){tab=next;render();window.scrollTo({top:0,behavior:'auto'})}
function clearSearch(){search='';const field=document.querySelector<HTMLInputElement>('#search');if(field)field.value='';renderList();field?.focus()}
function confirmDialog(title:string,description:string,label:string,destructive=true):Promise<boolean>{
 return new Promise(resolve=>{
  const dialog=el('dialog','modal confirm-modal');const form=el('form');form.method='dialog';
  const heading=el('h2','',title);heading.id='confirm-title';
  dialog.setAttribute('aria-labelledby',heading.id);
  const detail=el('p','muted',description);detail.id='confirm-description';dialog.setAttribute('aria-describedby',detail.id);
  const footer=el('div','modal-actions');
  const cancel=el('button','button secondary','取消');cancel.value='cancel';
  const ok=el('button',destructive?'button danger-solid':'button primary',label);ok.value='ok';
  footer.append(cancel,ok);form.append(heading,detail,footer);dialog.append(form);document.body.append(dialog);
  dialog.addEventListener('close',()=>{const accepted=dialog.returnValue==='ok';dialog.remove();resolve(accepted)},{once:true});
  dialog.showModal();cancel.focus();
 });
}

function bytes(n:number):string{if(!Number.isFinite(n))return '—';if(n===0)return '0 B';const units=['B','KiB','MiB','GiB','TiB'];const i=Math.min(Math.floor(Math.log(Math.max(n,1))/Math.log(1024)),4);return (n/1024**i).toLocaleString('zh-CN',{maximumFractionDigits:i>0?1:0})+' '+units[i]}
function date(s:string){return new Date(s).toLocaleString('zh-CN',{year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'})}
function absolute(s:string){return new URL(s,location.origin).href}
function message(s:string,error=false){const n=el('div','toast'+(error?' error':''),s);n.title=s;document.querySelector('#notifications')!.replaceChildren(n);setTimeout(()=>n.remove(),5500)}
function errorText(e:unknown){return e instanceof Error&&/[\u4e00-\u9fff]/.test(e.message)?e.message:'网络或服务暂时不可用，请稍后重试'}
async function api<T>(path:string,method='GET',data?:unknown):Promise<T>{
 const h:Record<string,string>={};if(method!=='GET')h['X-CSRF-Token']=session?.csrf||'';
 if(data!==undefined)h['Content-Type']='application/json';
 const r=await fetch(path,{method,headers:h,credentials:'same-origin',body:data===undefined?undefined:JSON.stringify(data)});
 const value=await r.json().catch(()=>({error:'服务器返回了无法读取的响应'}));
 if(!r.ok){if(r.status===401&&path!=='/api/login'&&session){session=null;loginView('登录已过期，请重新登录后继续。')}throw new Error(value.error||'操作失败（'+r.status+'）')}
 return value as T;
}
async function copy(text:string){try{await navigator.clipboard.writeText(absolute(text));message('链接已复制')}catch{await promptDialog('复制链接','请复制下面的链接',absolute(text),'关闭',false)}}
function promptDialog(title:string,label:string,initial='',confirmLabel='确定',editable=true,options?:[string,string][]):Promise<string|null>{
 return new Promise(resolve=>{
  const dialog=el('dialog','modal');const form=el('form');form.method='dialog';
  const heading=el('h2','',title);heading.id='prompt-title';dialog.setAttribute('aria-labelledby',heading.id);
  form.append(heading,el('p','muted',label));
  let input:HTMLInputElement|HTMLSelectElement;
  if(options){const select=el('select','field');for(const [value,text] of options){const o=el('option','',text);o.value=value;select.append(o)}input=select;input.value=initial}
  else{input=el('input','field');input.value=initial;input.readOnly=!editable}
  input.setAttribute('aria-label',label);
  const footer=el('div','modal-actions');const cancel=el('button','button secondary','取消');cancel.value='cancel';const ok=el('button','button primary',confirmLabel);ok.value='ok';footer.append(cancel,ok);
  form.append(input,footer);dialog.append(form);document.body.append(dialog);
  dialog.addEventListener('close',()=>{const value=dialog.returnValue==='ok'?input.value:null;dialog.remove();resolve(value)},{once:true});
  dialog.showModal();input.focus();if(input instanceof HTMLInputElement){input.select();input.addEventListener('keydown',event=>{if(event.key==='Enter'&&!event.isComposing){event.preventDefault();form.requestSubmit(ok)}})}
 });
}
function loginView(note=''){
 if(refreshTimer)clearInterval(refreshTimer);
 for(const j of jobs){if(j.state==='uploading'){void j.upload?.abort();j.state='paused'}}
 root.innerHTML='<main class="login-layout"><section class="login-intro"><a class="brand" href="/"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><div class="intro-copy"><span class="eyebrow">YOUR PRIVATE FILE SPACE</span><h1>文件归你。<br>分享，由你。</h1><p>把大文件安心放在自己的服务器。<br>随时续传，随时分享。</p><div class="intro-tags"><span>断点续传</span><span>私密存储</span><span>直接下载</span></div></div><p class="intro-foot">一个人管理，一条链接分享。</p></section><section class="login-card"><div class="login-card-inner"><span class="eyebrow">WELCOME BACK</span><h2>打开你的文件空间</h2><p class="muted">使用管理员账号登录</p><form id="login-form"><label for="username">用户名</label><input class="field" id="username" name="username" autocomplete="username" autocapitalize="none" spellcheck="false" required value="admin"><label for="password">密码</label><div class="password-wrap"><input class="field" id="password" name="password" type="password" autocomplete="current-password" required><button id="password-toggle" class="text-button" type="button" aria-label="显示密码" aria-pressed="false">显示</button></div><p id="login-error" class="form-error" role="alert"></p><button class="button primary full" type="submit">登录网盘 <span aria-hidden="true">↗</span></button></form><p class="login-note">文件管理仅对你开放。分享接收者无需登录。</p></div></section></main>';
 document.title='登录 · Solo Drive';
 document.querySelector('#login-error')!.textContent=note;
 document.querySelector<HTMLButtonElement>('#password-toggle')!.onclick=()=>{
  const input=document.querySelector<HTMLInputElement>('#password')!;const button=document.querySelector<HTMLButtonElement>('#password-toggle')!;
  const visible=input.type==='password';input.type=visible?'text':'password';button.textContent=visible?'隐藏':'显示';button.setAttribute('aria-label',visible?'隐藏密码':'显示密码');button.setAttribute('aria-pressed',String(visible));
 };
 document.querySelector<HTMLFormElement>('#login-form')!.onsubmit=async e=>{
  e.preventDefault();const button=document.querySelector<HTMLButtonElement>('#login-form button[type="submit"]')!;button.disabled=true;
  try{session=await api<Session>('/api/login','POST',{username:(document.querySelector('#username') as HTMLInputElement).value,password:(document.querySelector('#password') as HTMLInputElement).value});await dashboard()}
  catch(e){document.querySelector('#login-error')!.textContent=errorText(e)}finally{button.disabled=false}
 };
}
async function dashboard(){
 listSnapshot='';capacitySnapshot='';loaded=false;
 root.innerHTML='<a href="#main-content" class="skip-link">跳到文件内容</a><div class="shell"><aside class="sidebar"><a class="brand" href="/" aria-label="Solo Drive 首页"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><div class="workspace-tag"><span class="status-dot"></span> 私人文件空间</div><nav aria-label="主导航"><button data-tab="files"><span aria-hidden="true">'+icon('files')+'</span><span class="nav-label">全部文件</span><b id="file-count">0</b></button><button data-tab="uploads"><span aria-hidden="true">'+icon('upload')+'</span><span class="nav-label">传输任务</span><b id="upload-count">0</b></button><button data-tab="shares"><span aria-hidden="true">'+icon('share')+'</span><span class="nav-label">我的分享</span><b id="share-count">0</b></button></nav><div class="sidebar-bottom"><div class="owner"><span class="avatar">A</span><div><strong id="owner-name"></strong><small>管理员</small></div></div><button id="logout" class="text-button">退出登录</button></div></aside><main id="main-content" class="main" tabindex="-1"><header class="page-header"><div><span class="eyebrow">MY FILE SPACE</span><h1 id="page-title">全部文件</h1><p id="page-subtitle">所有文件，一处管理。</p></div><button id="upload-button" class="button primary">'+icon('upload')+' 上传文件</button></header><section id="capacity" class="capacity" aria-label="存储空间"><p class="capacity-loading muted">正在读取存储空间…</p></section><section class="content-card"><div id="toolbar" class="toolbar"><div class="list-heading"><h2 id="list-title">文件列表</h2><span id="list-meta" class="list-meta"></span></div><div class="list-tools"><div id="search-wrap" class="search-wrap"><input id="search" class="search" type="search" aria-label="搜索文件" aria-keyshortcuts="/" title="搜索文件（快捷键 /）" placeholder="搜索文件名" autocomplete="off"><button id="clear-search" class="icon-button search-clear" aria-label="清空搜索" hidden>'+icon('close')+'</button></div><select id="sort" aria-label="排序"><option value="new">最近上传</option><option value="name">文件名称</option><option value="size">文件大小</option></select><button id="refresh" class="icon-button" title="刷新列表" aria-label="刷新">'+icon('refresh')+'</button></div></div><div id="dropzone" class="dropzone" tabindex="0" role="button" aria-label="选择文件上传"><span class="drop-icon">'+icon('upload')+'</span><div><strong class="desktop-label">拖放文件到这里，或点击上传</strong><strong class="mobile-label">轻点选择文件</strong><p>支持断点续传，可一次选择多个文件</p></div><span class="drop-tip">选择文件</span></div><div id="view" aria-busy="true"><div class="empty"><p class="muted">正在加载文件…</p></div></div></section><p id="sync-status" class="sync-status" role="status"></p><p class="page-foot">文件由你保管，分享由你决定。</p></main></div><input id="file-picker" type="file" multiple hidden>';
 document.querySelector('#owner-name')!.textContent=session!.username;
 document.querySelectorAll<HTMLButtonElement>('[data-tab]').forEach(b=>b.onclick=()=>switchTab(b.dataset.tab!));
 document.querySelector<HTMLButtonElement>('#upload-button')!.onclick=()=>pickFiles();
 document.querySelector<HTMLInputElement>('#file-picker')!.onchange=e=>{const input=e.target as HTMLInputElement;const selected=Array.from(input.files||[]);const key=input.dataset.resume;input.value='';delete input.dataset.resume;void addFiles(selected,key)};
 const zone=document.querySelector<HTMLElement>('#dropzone')!;
 zone.onclick=()=>pickFiles();zone.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();pickFiles()}};
 zone.ondragover=e=>{e.preventDefault();zone.classList.add('dragging')};zone.ondragleave=()=>zone.classList.remove('dragging');zone.ondrop=e=>{e.preventDefault();zone.classList.remove('dragging');void addFiles(Array.from(e.dataTransfer?.files||[]))};
 const searchInput=document.querySelector<HTMLInputElement>('#search')!;searchInput.value=search;
 searchInput.oninput=e=>{search=(e.target as HTMLInputElement).value;renderList()};
 document.querySelector<HTMLButtonElement>('#clear-search')!.onclick=clearSearch;
 const sortInput=document.querySelector<HTMLSelectElement>('#sort')!;sortInput.value=sort;
 sortInput.onchange=e=>{sort=(e.target as HTMLSelectElement).value;renderList()};
 document.querySelector<HTMLButtonElement>('#refresh')!.onclick=()=>void refresh();
 document.querySelector<HTMLButtonElement>('#logout')!.onclick=async()=>{
  if(jobs.some(j=>j.state==='uploading')&&!await confirmDialog('退出登录','当前上传将暂停，重新登录后可以继续。','确认退出',false))return;
  try{await api('/api/logout','POST');session=null;loginView()}catch(e){message(errorText(e),true)}
 };
 await refresh();if(session)refreshTimer=window.setInterval(()=>void refresh(false),5000);
}
function pickFiles(key?:string){const input=document.querySelector<HTMLInputElement>('#file-picker')!;input.multiple=!key;if(key)input.dataset.resume=key;else delete input.dataset.resume;input.click()}
async function refresh(notify=true){
 if(refreshBusy||!session)return;refreshBusy=true;
 const refreshButton=document.querySelector<HTMLButtonElement>('#refresh');
 if(refreshButton){refreshButton.disabled=true;if(notify)refreshButton.classList.add('refreshing')}
 try{
  const [f,c,s,u]=await Promise.all([api<{files:Entry[]}>('/api/files'),api<Capacity>('/api/storage'),api<{shares:Share[]}>('/api/shares'),api<{uploads:RemoteUpload[]}>('/api/uploads')]);
  if(!session)return;files=f.files;capacity=c;shares=s.shares;
  for(const remote of u.uploads){if(!jobs.some(j=>j.url&&new URL(j.url,location.origin).pathname.endsWith('/'+remote.id))){jobs.push({key:crypto.randomUUID(),name:remote.name,size:remote.size,offset:remote.offset,state:'waiting',url:remote.upload_url,speed:0,at:0,last:0})}}
  jobs=jobs.filter(j=>j.state!=='waiting'||u.uploads.some(x=>j.url&&new URL(j.url,location.origin).pathname.endsWith('/'+x.id)));
  loaded=true;render();
  const status=document.querySelector('#sync-status');if(status){status.textContent='';status.classList.remove('error')}
 }catch(e){
  const status=document.querySelector('#sync-status');if(status){status.textContent='列表暂时无法更新，请点击刷新重试。';status.classList.add('error')}
  if(!loaded){const host=document.querySelector('#view');if(host){host.setAttribute('aria-busy','false');host.replaceChildren(empty('暂时无法加载','请检查网络，然后重新加载。','重新加载',()=>refresh()))}}
  if(notify)message(errorText(e),true);
 }finally{refreshBusy=false;if(refreshButton){refreshButton.disabled=false;refreshButton.classList.remove('refreshing')}}
}
function render(){
 if(!document.querySelector('#view'))return;
 const title={files:'全部文件',uploads:'传输任务',shares:'我的分享'}[tab]||'全部文件';
 document.title=title+' · Solo Drive';
 document.querySelector('#page-title')!.textContent=title;
 document.querySelector('#page-subtitle')!.textContent={files:'所有文件，一处管理。',uploads:'随时暂停，下次接着传。',shares:'复制链接，即可分享给任何人。'}[tab]||'';
 document.querySelector('#list-title')!.textContent=title;
 document.querySelectorAll<HTMLButtonElement>('[data-tab]').forEach(b=>{b.classList.toggle('active',b.dataset.tab===tab);if(b.dataset.tab===tab)b.setAttribute('aria-current','page');else b.removeAttribute('aria-current')});
 document.querySelector('#file-count')!.textContent=String(files.length);
 document.querySelector('#upload-count')!.textContent=String(jobs.filter(j=>j.state!=='done').length);
 document.querySelector('#share-count')!.textContent=String(shares.length);
 (document.querySelector('#dropzone') as HTMLElement).hidden=tab==='shares';
 document.querySelector('#dropzone')!.classList.toggle('compact',tab==='files'&&files.length>0);
 (document.querySelector('#search-wrap') as HTMLElement).hidden=tab!=='files';
 (document.querySelector('#sort') as HTMLElement).hidden=tab!=='files';
 renderCapacity();renderList();updateProgress();
}
function renderCapacity(){
 const host=document.querySelector('#capacity')!;if(!capacity)return;
 const snapshot=JSON.stringify(capacity);if(snapshot===capacitySnapshot)return;capacitySnapshot=snapshot;
 host.replaceChildren();
 const c=capacity;const primary=el('div','capacity-main');const top=el('div','capacity-top');top.append(el('span','muted','可上传空间'),el('span','badge','实时容量'));
 const headline=el('div','capacity-number');headline.append(el('strong','',bytes(c.upload_available)),el('span','','可上传'));
 const track=el('div','meter');track.setAttribute('role','meter');track.setAttribute('aria-label','磁盘已用空间');track.setAttribute('aria-valuemin','0');track.setAttribute('aria-valuemax',String(c.total));track.setAttribute('aria-valuenow',String(Math.min(c.used,c.total)));track.setAttribute('aria-valuetext','已用 '+bytes(c.used)+'，共 '+bytes(c.total));
 const fill=el('div');fill.style.width=Math.min(100,c.total?c.used/c.total*100:0)+'%';track.append(fill);
 const support=el('p','capacity-support','磁盘剩余 '+bytes(c.available)+' / 总容量 '+bytes(c.total));
 primary.append(top,headline,track,support);
 const details=el('details','capacity-details');details.open=capacityExpanded;
 details.append(el('summary','','空间详情'));details.addEventListener('toggle',()=>{capacityExpanded=details.open});
 const secondary=el('div','capacity-stats');
 for(const [label,value,help] of [['网盘已占用',c.files_bytes+c.partial_bytes,'包含未完成的上传'],['系统保留',c.reserve,'留给系统和其他服务'],['待传预留',c.pending_bytes,'取消任务后可释放']] as [string,number,string][]){
  const item=el('div','capacity-stat');item.append(el('span','muted',label),el('strong','',bytes(value)),el('small','',help));secondary.append(item);
 }
 details.append(secondary,el('p','capacity-detail','可上传空间已扣除系统保留和待传预留。'));
 host.append(primary,details);
}
function empty(title:string,description:string,button?:string,fn?:()=>unknown){
 const box=el('div','empty');const symbol=el('div','empty-symbol');symbol.innerHTML=icon(tab==='shares'?'share':'upload');
 box.append(symbol,el('h3','',title),el('p','muted',description));if(button&&fn)box.append(action(button,fn,'button secondary'));return box;
}
function eta(j:Job){
 if(j.state!=='uploading')return '';
 if(j.speed<=0||j.offset<=0)return '正在准备…';
 const seconds=Math.ceil(Math.max(0,j.size-j.offset)/j.speed);
 return seconds<60?'预计不到 1 分钟':seconds<3600?'预计 '+Math.ceil(seconds/60)+' 分钟':'预计 '+Math.floor(seconds/3600)+' 小时 '+Math.ceil(seconds%3600/60)+' 分钟';
}
function renderList(){
 const host=document.querySelector('#view');if(!host)return;
 const clear=document.querySelector<HTMLButtonElement>('#clear-search');if(clear)clear.hidden=!search;
 const filtered=files.filter(f=>f.name.toLocaleLowerCase().includes(search.toLocaleLowerCase())).sort((a,b)=>sort==='name'?a.name.localeCompare(b.name):sort==='size'?b.size-a.size:b.created_at.localeCompare(a.created_at));
 document.querySelector('#list-meta')!.textContent=tab==='files'?(search?filtered.length+' / '+files.length+' 个文件':files.length+' 个文件'):tab==='shares'?shares.length+' 条链接':jobs.length+' 项任务';
 host.setAttribute('aria-busy',String(!loaded));
 if(!loaded)return;
 // Progress is patched separately; polling must not destroy a focused control
 // or an open file menu when the underlying list is unchanged.
 const snapshot=JSON.stringify([tab,search,sort,tab==='files'?filtered:tab==='shares'?shares.map(s=>[s,!!s.expires_at&&Date.parse(s.expires_at)<Date.now()]):jobs.map(j=>[j.key,j.name,j.size,j.state,j.error,!!j.file])]);
 if(snapshot===listSnapshot)return;listSnapshot=snapshot;
 host.replaceChildren();
 if(tab==='files'){
  if(!filtered.length){host.append(empty(search?'没有匹配的文件':'你的文件空间，准备好了',search?'换个关键词，或清空搜索查看全部文件。':'上传文件后，就能一键创建分享链接。',search?'查看全部文件':'上传文件',search?clearSearch:()=>pickFiles()));return}
  const table=el('div','file-table');const head=el('div','file-row table-head');for(const text of ['文件名称','大小','上传时间','操作'])head.append(el('span','',text));table.append(head);
  for(const f of filtered){
   const row=el('div','file-row');row.dataset.file=f.id;
   const name=el('div','file-name');const symbol=el('span','file-symbol',f.name.includes('.')?f.name.split('.').pop()?.slice(0,4).toUpperCase()||'FILE':'FILE');symbol.setAttribute('aria-hidden','true');
   const text=el('strong','file-title',f.name);text.title=f.name;name.append(symbol,text);
   const actions=el('div','row-actions');const download=el('a','text-button','下载');download.href='/api/files/'+f.id+'/download';download.setAttribute('download','');
   const menu=el('details','file-menu');const more=el('summary','icon-button');more.innerHTML=icon('more');more.setAttribute('aria-label','更多操作：'+f.name);more.title='更多操作';
   const popover=el('div','file-menu-popover');
   popover.append(action('重命名',()=>{menu.open=false;return renameFile(f)},'text-button'),action('删除',()=>{menu.open=false;return deleteFile(f)},'text-button danger'));
   menu.append(more,popover);
   menu.addEventListener('toggle',()=>{
    if(menu.open){
     document.querySelectorAll<HTMLDetailsElement>('.file-menu[open]').forEach(other=>{if(other!==menu)other.open=false});
     const bottom=more.getBoundingClientRect().bottom;const safeBottom=window.matchMedia('(max-width:700px)').matches?90:16;
     menu.classList.toggle('open-up',bottom+popover.offsetHeight+8>window.innerHeight-safeBottom);
    }
   });
   actions.append(download,action('分享',()=>shareFile(f),'text-button'),menu);
   row.append(name,el('span','file-size',bytes(f.size)),el('span','file-date',date(f.created_at)),actions);table.append(row);
  }host.append(table);
 }else if(tab==='shares'){
  if(!shares.length){host.append(empty('还没有分享链接','选择一个文件，点击「分享」即可创建链接。','去选文件',()=>switchTab('files')));return}
  for(const s of shares){
   const card=el('article','share-row');const info=el('div','share-info');const expired=s.expires_at&&Date.parse(s.expires_at)<Date.now();
   info.append(el('strong','file-title',s.file_name),el('p','muted',bytes(s.size)+' · '+(s.expires_at?(expired?'已过期 · ':'到期 ')+date(s.expires_at):'永久有效')));
   if(expired)card.classList.add('expired');
   const link=el('a','share-url truncate',absolute(s.url));link.href=absolute(s.url);link.target='_blank';link.rel='noopener noreferrer';link.title='打开分享页面';info.append(link);
   const actions=el('div','row-actions');actions.append(action('复制分享',()=>copy(s.url),'button secondary small'),action('复制直链',()=>copy(s.download_url),'button secondary small'),action('撤销',()=>revokeShare(s),'text-button danger'));card.append(info,actions);host.append(card);
  }
 }else{
  if(!jobs.length){host.append(empty('暂无传输任务','选择文件开始上传，暂停后仍会保留进度。','选择文件',()=>pickFiles()));return}
  const active=jobs.filter(j=>j.state==='uploading'||j.state==='queued');const pending=jobs.filter(j=>['paused','waiting','error'].includes(j.state));const done=jobs.filter(j=>j.state==='done');
  const overview=el('div','transfer-summary');
  for(const [label,count] of [['传输中',active.length],['待继续',pending.length],['已完成',done.length]] as [string,number][]){const stat=el('div','transfer-stat');stat.append(el('strong','',String(count)),el('span','muted',label));overview.append(stat)}host.append(overview);
  const summary=el('div','queue-summary');const queueActions=el('div','queue-actions');
  if(active.length)queueActions.append(action('全部暂停',pauseAll,'text-button'));
  if(pending.some(j=>j.upload))queueActions.append(action('继续全部',()=>{for(const j of pending){if(j.upload){j.error=undefined;j.state='queued'}}pump()},'text-button'));
  if(done.length)queueActions.append(action('清除已完成',()=>{jobs=jobs.filter(j=>j.state!=='done');render()},'text-button'));
  summary.append(el('span','muted','最多同时上传 2 个文件'),queueActions);host.append(summary);
  if(jobs.some(j=>j.state==='waiting'))host.append(el('p','resume-hint','页面重新打开后，请选择原文件继续上传，已传部分不会重传。'));
  for(const j of jobs){
   const row=el('article','upload-row');row.dataset.job=j.key;
   const top=el('div','upload-top');top.append(el('strong','file-title',j.name),el('span','upload-state '+j.state,labels[j.state]));
   const progress=el('progress');progress.max=j.size||1;progress.value=j.state==='done'?progress.max:j.offset;progress.setAttribute('aria-label',j.name+' 上传进度');
   const line=el('div','upload-progress-line');line.append(el('span','progress-percent',Math.floor(j.state==='done'?100:j.size?Math.min(100,j.offset/j.size*100):0)+'%'),el('span','progress-eta',eta(j)));
   const bottom=el('div','upload-bottom');const text=el('span','muted',bytes(j.offset)+' / '+bytes(j.size)+(j.state==='uploading'?' · '+bytes(j.speed)+'/s':''));
   const controls=el('div','row-actions');
   if(j.state==='uploading')controls.append(action('暂停',()=>pause(j),'text-button'));
   if(j.state==='paused'||j.state==='error')controls.append(action(j.file?'继续上传':'选择原文件',()=>j.file?resume(j):pickFiles(j.key),'text-button'));
   if(j.state==='waiting')controls.append(action('选择原文件续传',()=>pickFiles(j.key),'text-button'));
   if(j.state!=='done')controls.append(action('取消',()=>cancel(j),'text-button danger'));
   else controls.append(action('查看文件',()=>{search=j.name;document.querySelector<HTMLInputElement>('#search')!.value=search;switchTab('files')},'text-button'));
   bottom.append(text,controls);row.append(top,progress,line,bottom);
   if(j.error)row.append(el('p','form-error',j.error));host.append(row);
  }
 }
}
async function renameFile(f:Entry){const name=await promptDialog('重命名文件','文件名称',f.name);if(name===null||name===f.name)return;try{await api('/api/files/'+f.id+'/rename','POST',{name});message('文件已重命名');await refresh()}catch(e){message(errorText(e),true)}}
async function deleteFile(f:Entry){if(!await confirmDialog('删除文件','永久删除「'+f.name+'」？该文件的所有分享也会失效。','确认删除'))return;try{await api('/api/files/'+f.id,'DELETE');message('文件已删除');await refresh()}catch(e){message(errorText(e),true)}}
async function shareFile(f:Entry){const hours=await promptDialog('创建分享链接',f.name,'0','创建分享',true,[['0','永久有效'],['24','1 天'],['168','7 天'],['720','30 天']]);if(hours===null)return;try{const s=await api<Share>('/api/files/'+f.id+'/shares','POST',{expires_in_hours:Number(hours)});await refresh();tab='shares';render();await copy(s.url)}catch(e){message(errorText(e),true)}}
async function revokeShare(s:Share){if(!await confirmDialog('撤销分享','「'+s.file_name+'」的链接将立即失效，正在进行的下载也会停止。','确认撤销'))return;try{await api('/api/shares/'+s.id,'DELETE');message('分享已撤销');await refresh()}catch(e){message(errorText(e),true)}}

function updateProgress(){if(renderPending)return;renderPending=true;setTimeout(()=>{
 renderPending=false;if(tab!=='uploads')return;
 for(const job of jobs){const row=document.querySelector<HTMLElement>('[data-job="'+job.key+'"]');if(!row)continue;
 const progress=row.querySelector('progress');if(progress){progress.max=job.size||1;progress.value=job.state==='done'?progress.max:job.offset}
 const percent=row.querySelector('.progress-percent');if(percent)percent.textContent=Math.floor(job.state==='done'?100:job.size?Math.min(100,job.offset/job.size*100):0)+'%';
 const remaining=row.querySelector('.progress-eta');if(remaining)remaining.textContent=eta(job);
 const text=row.querySelector('.upload-bottom>.muted');if(text)text.textContent=bytes(job.offset)+' / '+bytes(job.size)+(job.state==='uploading'?' · '+bytes(job.speed)+'/s':'');
 }
},200)}
async function addFiles(selected:File[],resumeKey?:string){
 if(!selected.length)return;
 if(!crypto.subtle){message('请通过 HTTPS 或本机 localhost 打开网盘，以启用上传校验。',true);return}
 for(const file of selected){
  let j:Job|undefined=resumeKey?jobs.find(x=>x.key===resumeKey):undefined;
  if(j){if(file.name!==j.name||file.size!==j.size){message('请选择名称和大小均一致的原文件。',true);continue}
   if(!await confirmDialog('继续上传','请确认这是「'+j.name+'」上传时的原文件，内容未被修改。','确认续传',false))continue;
  }else{j={key:crypto.randomUUID(),name:file.name,size:file.size,offset:0,state:'queued',speed:0,at:0,last:0}}
  const job=j;job.file=file;job.error=undefined;
  const upload=new tus.Upload(file,{
   endpoint:'/api/uploads/',chunkSize,parallelUploads:1,retryDelays:[0,1000,3000,5000,10000,20000],removeFingerprintOnSuccess:true,
   metadata:{filename:file.name,filetype:file.type,last_modified:String(file.lastModified)},
   onBeforeRequest:async req=>{
    const generation=job.generation;if(job.state!=='uploading')throw new Error('SOLODRIVE_PAUSED');
    req.setHeader('X-CSRF-Token',session?.csrf||'');
    if(req.getMethod()==='PATCH'){
     const offset=Number(req.getHeader('Upload-Offset'));if(!Number.isSafeInteger(offset)||offset<0)throw new Error('上传偏移无效');
     const hash=await crypto.subtle.digest('SHA-256',await file.slice(offset,Math.min(file.size,offset+chunkSize)).arrayBuffer());
     if(job.state!=='uploading'||job.generation!==generation)throw new Error('SOLODRIVE_PAUSED');
     req.setHeader('Upload-Checksum','sha256 '+btoa(String.fromCharCode(...new Uint8Array(hash))));
    }
   },
   onShouldRetry:(err,attempt)=>{if(job.state!=='uploading'||err.message.includes('SOLODRIVE_PAUSED'))return false;const status=err.originalResponse?.getStatus()||0;return attempt<6&&(status===0||status===409||status===423||status===429||status===460||status>=500&&status!==507)},
   onProgress:(sent,total)=>{if(job.state!=='uploading')return;const now=performance.now();if(job.at&&now-job.at>200){job.speed=Math.max(0,(sent-job.last)/((now-job.at)/1000));job.last=sent;job.at=now}else if(!job.at){job.at=now;job.last=sent}job.offset=sent;job.size=total;job.url=upload.url||job.url;updateProgress()},
   onError:err=>{if(job.state==='paused'||err.message.includes('SOLODRIVE_PAUSED'))return;job.state='error';const status='originalResponse' in err?err.originalResponse?.getStatus():undefined;job.error=status===507?'磁盘空间不足。请清理文件或取消其他任务后重试。':status===401?'登录已过期，请重新登录后继续。':status===460?'上传块校验未通过，请重试。':'上传暂时失败，可继续重试。';job.speed=0;render();pump()},
   onSuccess:()=>{job.state='done';job.offset=file.size;job.url=upload.url||job.url;job.speed=0;job.error=undefined;message('上传完成：'+job.name);void refresh(false);render();pump()}
  });
  job.upload=upload;
  try{
   if(job.url){upload.options.uploadUrl=absolute(job.url)}
   else{
    const previous=await upload.findPreviousUploads();
    for(const old of previous){
     const pending=jobs.find(x=>x.state==='waiting'&&x.url&&old.uploadUrl&&new URL(x.url,location.origin).pathname===new URL(old.uploadUrl,location.origin).pathname);
     if(pending){job.key=pending.key;job.offset=pending.offset;job.url=pending.url;jobs=jobs.filter(x=>x!==pending);upload.resumeFromPreviousUpload(old);break}
    }
   }
   job.state='queued';if(!jobs.includes(job))jobs.push(job);
  }catch(e){job.state='error';job.error=errorText(e);if(!jobs.includes(job))jobs.push(job)}
 }
 tab='uploads';render();pump();
}
function pump(){if(!session)return;let count=jobs.filter(j=>j.state==='uploading').length;for(const j of jobs){if(count>=2)break;if(j.state==='queued'&&j.upload){j.state='uploading';j.generation=(j.generation||0)+1;j.at=0;j.speed=0;count++;j.upload.start()}}render()}
async function pauseAll(){
 const active=jobs.filter(j=>j.state==='uploading'||j.state==='queued');
 for(const j of active){j.generation=(j.generation||0)+1;j.state='paused';j.speed=0}
 render();await Promise.all(active.map(j=>j.upload?.abort()));render();
}
async function pause(j:Job){j.generation=(j.generation||0)+1;j.state='paused';await j.upload?.abort();j.speed=0;render();pump()}
function resume(j:Job){j.error=undefined;if(!j.upload){pickFiles(j.key);return}j.state='queued';render();pump()}
async function cancel(j:Job){
 if(!await confirmDialog('取消上传','取消「'+j.name+'」并删除已上传部分？仅想稍后继续，请使用暂停。','确认取消'))return;
 if(!jobs.includes(j))return;
 const completed=()=>{j.state='done';j.offset=j.size;j.speed=0;j.error=undefined;message('文件已上传完成，如需删除请到全部文件。');render();pump()};
 if(j.state==='done'){completed();return}
 try{
  j.generation=(j.generation||0)+1;j.state='paused';await j.upload?.abort();
  // A transfer may finish while its confirmation dialog or abort is pending.
  if((j.state as string)==='done'){completed();return}
  const url=j.upload?.url||j.url;
  if(url){
   const r=await fetch(absolute(url),{method:'DELETE',headers:{'Tus-Resumable':'1.0.0','X-CSRF-Token':session?.csrf||''},credentials:'same-origin'});
   if(r.status===409){
    const current=await api<{files:Entry[]}>('/api/files');
    const id=new URL(url,location.origin).pathname.split('/').pop();
    if(current.files.some(f=>f.id===id)){files=current.files;completed();void refresh(false);return}
   }
   if(!r.ok&&r.status!==404)throw new Error('取消失败，请重试');
  }
  jobs=jobs.filter(x=>x!==j);message('上传任务已取消');await refresh(false);pump();
 }catch(e){j.state='error';j.error=errorText(e);render()}
}
async function shareView(token:string){
 document.title='文件分享 · Solo Drive';
 root.innerHTML='<main class="public-layout"><a class="brand" href="/"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><section id="share-card" class="public-card" aria-live="polite"></section><p class="muted public-foot">私人存储，直接分享。</p></main>';
 const card=document.querySelector('#share-card')!;card.append(el('p','muted','正在读取分享…'));
 try{const s=await api<{file_name:string;size:number;expires_at:string|null;download_url:string}>('/api/public/shares/'+encodeURIComponent(token));
  card.replaceChildren();document.title=s.file_name+' · Solo Drive';
  card.append(el('span','eyebrow','分享给你的文件'),el('div','public-file-icon','↓'),el('h1','public-name',s.file_name),el('p','muted',bytes(s.size)+' · '+(s.expires_at?'有效至 '+date(s.expires_at):'链接长期有效')));
  const link=el('a','button primary full','下载文件');link.href=absolute(s.download_url);link.setAttribute('download','');card.append(link,action('复制下载直链',()=>void copy(s.download_url),'button secondary full'),el('p','download-note','支持断点下载。将直链粘贴到下载器，可使用多连接下载。'));
 }catch(e){card.replaceChildren();card.append(el('span','eyebrow','分享已失效'),el('h1','','分享链接不可用'),el('p','muted',errorText(e)))}
}
async function boot(){
 const match=location.pathname.match(/^\/s\/([^/]+)\/?$/);if(match){await shareView(match[1]);return}
 try{session=await api<Session>('/api/session');await dashboard()}catch{loginView()}
}
document.addEventListener('click',event=>{
 const target=event.target as Element;
 document.querySelectorAll<HTMLDetailsElement>('.file-menu[open]').forEach(menu=>{if(!menu.contains(target))menu.open=false});
});
document.addEventListener('keydown',event=>{
 if(document.querySelector('dialog[open]'))return;
 if(event.key==='Escape'){
  const menu=document.querySelector<HTMLDetailsElement>('.file-menu[open]');
  if(menu){menu.open=false;menu.querySelector<HTMLElement>('summary')?.focus();event.preventDefault()}
 }
 const target=event.target as HTMLElement;
 if(event.key==='/'&&!event.ctrlKey&&!event.metaKey&&!event.altKey&&session&&!target.closest('input,textarea,select,[contenteditable="true"]')){
  event.preventDefault();if(tab!=='files')switchTab('files');document.querySelector<HTMLInputElement>('#search')?.focus();
 }
});
void boot();
