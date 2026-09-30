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
const chunkSize=8*1024*1024;
const labels:Record<string,string>={queued:'排队中',uploading:'上传中',paused:'已暂停',waiting:'等待选择原文件',done:'已完成',error:'需重试'};

function el<K extends keyof HTMLElementTagNameMap>(tag:K, cls='',text=''):HTMLElementTagNameMap[K]{
 const n=document.createElement(tag);n.className=cls;if(text)n.textContent=text;return n;
}
function action(text:string,fn:()=>unknown,cls='button secondary'){const b=el('button',cls,text);b.type='button';b.addEventListener('click',()=>void fn());return b}
function bytes(n:number):string{if(!Number.isFinite(n))return '—';if(n===0)return '0 B';const units=['B','KiB','MiB','GiB','TiB'];const i=Math.min(Math.floor(Math.log(Math.max(n,1))/Math.log(1024)),4);return (n/1024**i).toLocaleString('zh-CN',{maximumFractionDigits:i>0?1:0})+' '+units[i]}
function date(s:string){return new Date(s).toLocaleString('zh-CN',{year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'})}
function absolute(s:string){return new URL(s,location.origin).href}
function message(s:string,error=false){const n=el('div','toast'+(error?' error':''),s);document.querySelector('#notifications')!.append(n);setTimeout(()=>n.remove(),5500)}
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
  form.append(el('h2','',title),el('p','muted',label));
  let input:HTMLInputElement|HTMLSelectElement;
  if(options){const select=el('select','field');for(const [value,text] of options){const o=el('option','',text);o.value=value;select.append(o)}input=select;input.value=initial}
  else{input=el('input','field');input.value=initial;input.readOnly=!editable}
  input.setAttribute('aria-label',label);
  const footer=el('div','modal-actions');const cancel=el('button','button secondary','取消');cancel.value='cancel';const ok=el('button','button primary',confirmLabel);ok.value='ok';footer.append(cancel,ok);
  form.append(input,footer);dialog.append(form);document.body.append(dialog);
  dialog.addEventListener('close',()=>{const value=dialog.returnValue==='ok'?input.value:null;dialog.remove();resolve(value)},{once:true});
  dialog.showModal();input.focus();if(input instanceof HTMLInputElement)input.select();
 });
}
function loginView(note=''){
 if(refreshTimer)clearInterval(refreshTimer);
 for(const j of jobs){if(j.state==='uploading'){void j.upload?.abort();j.state='paused'}}
 root.innerHTML='<main class="login-layout"><section class="login-intro"><a class="brand" href="/"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><div class="intro-copy"><span class="eyebrow">YOUR PRIVATE FILE SPACE</span><h1>文件归你。<br>分享，由你。</h1><p>把大文件安心放在自己的服务器。<br>随时续传，随时分享。</p><div class="intro-tags"><span>断点续传</span><span>私密存储</span><span>直接下载</span></div></div><p class="intro-foot">一个人管理，一条链接分享。</p></section><section class="login-card"><div class="login-card-inner"><span class="eyebrow">WELCOME BACK</span><h2>打开你的文件空间</h2><p class="muted">使用管理员账号登录</p><form id="login-form"><label for="username">用户名</label><input class="field" id="username" name="username" autocomplete="username" required value="admin"><label for="password">密码</label><input class="field" id="password" name="password" type="password" autocomplete="current-password" required><p id="login-error" class="form-error" role="alert"></p><button class="button primary full" type="submit">登录网盘 <span aria-hidden="true">↗</span></button></form><p class="login-note">文件管理仅对你开放。分享接收者无需登录。</p></div></section></main>';
 document.querySelector('#login-error')!.textContent=note;
 document.querySelector<HTMLFormElement>('#login-form')!.onsubmit=async e=>{
  e.preventDefault();const button=document.querySelector<HTMLButtonElement>('#login-form button')!;button.disabled=true;
  try{session=await api<Session>('/api/login','POST',{username:(document.querySelector('#username') as HTMLInputElement).value,password:(document.querySelector('#password') as HTMLInputElement).value});await dashboard()}
  catch(e){document.querySelector('#login-error')!.textContent=errorText(e)}finally{button.disabled=false}
 };
}
async function dashboard(){
 root.innerHTML='<div class="shell"><aside class="sidebar"><a class="brand" href="/"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><div class="workspace-tag"><span class="status-dot"></span> 私人文件空间</div><nav aria-label="主导航"><button data-tab="files"><span aria-hidden="true">▤</span>全部文件 <b id="file-count">0</b></button><button data-tab="uploads"><span aria-hidden="true">↑</span>传输任务 <b id="upload-count">0</b></button><button data-tab="shares"><span aria-hidden="true">↗</span>我的分享 <b id="share-count">0</b></button></nav><div class="sidebar-bottom"><div class="owner"><span class="avatar">A</span><div><strong id="owner-name"></strong><small>管理员</small></div></div><button id="logout" class="text-button">退出登录</button></div></aside><main class="main"><header class="page-header"><div><span class="eyebrow">MY FILE SPACE</span><h1 id="page-title">全部文件</h1><p id="page-subtitle">有序存放，自由分享。</p></div><button id="upload-button" class="button primary"><span aria-hidden="true">＋</span> 上传文件</button></header><section id="capacity" class="capacity" aria-label="存储空间"></section><section class="content-card"><div id="toolbar" class="toolbar"><h2 id="list-title">文件列表</h2><div class="list-tools"><input id="search" class="search" type="search" aria-label="搜索文件" placeholder="搜索文件名…"><select id="sort" aria-label="排序"><option value="new">最近上传</option><option value="name">文件名称</option><option value="size">文件大小</option></select><button id="refresh" class="icon-button" title="刷新" aria-label="刷新"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5M6.3 7a7 7 0 0 1 11.9-.7L20 9M4 15l1.8 2.7A7 7 0 0 0 17.7 17"/></svg></button></div></div><div id="dropzone" class="dropzone" tabindex="0" role="button" aria-label="拖放文件或点击选择上传"><span class="drop-icon">↑</span><div><strong>将文件拖到这里，或点击上传</strong><p>支持大文件续传 · 页面重开后重新选择原文件即可继续</p></div><span class="drop-tip">本地文件</span></div><div id="view"></div></section><p class="page-foot">文件保存在你自己的服务器 · 上传与下载均不设置应用限速</p></main></div><input id="file-picker" type="file" multiple hidden>';
 document.querySelector('#owner-name')!.textContent=session!.username;
 document.querySelectorAll<HTMLButtonElement>('[data-tab]').forEach(b=>b.onclick=()=>{tab=b.dataset.tab!;render()});
 document.querySelector<HTMLButtonElement>('#upload-button')!.onclick=()=>pickFiles();
 document.querySelector<HTMLInputElement>('#file-picker')!.onchange=e=>{const input=e.target as HTMLInputElement;const selected=Array.from(input.files||[]);const key=input.dataset.resume;input.value='';delete input.dataset.resume;void addFiles(selected,key)};
 const zone=document.querySelector<HTMLElement>('#dropzone')!;
 zone.onclick=()=>pickFiles();zone.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();pickFiles()}};
 zone.ondragover=e=>{e.preventDefault();zone.classList.add('dragging')};zone.ondragleave=()=>zone.classList.remove('dragging');zone.ondrop=e=>{e.preventDefault();zone.classList.remove('dragging');void addFiles(Array.from(e.dataTransfer?.files||[]))};
 document.querySelector<HTMLInputElement>('#search')!.oninput=e=>{search=(e.target as HTMLInputElement).value;renderList()};
 document.querySelector<HTMLSelectElement>('#sort')!.onchange=e=>{sort=(e.target as HTMLSelectElement).value;renderList()};
 document.querySelector<HTMLButtonElement>('#refresh')!.onclick=()=>void refresh();
 document.querySelector<HTMLButtonElement>('#logout')!.onclick=async()=>{
  if(jobs.some(j=>j.state==='uploading')&&!confirm('退出将暂停当前上传，之后可以继续。确定退出？'))return;
  try{await api('/api/logout','POST');session=null;loginView()}catch(e){message(errorText(e),true)}
 };
 await refresh();if(session)refreshTimer=window.setInterval(()=>void refresh(false),5000);
}
function pickFiles(key?:string){const input=document.querySelector<HTMLInputElement>('#file-picker')!;input.multiple=!key;if(key)input.dataset.resume=key;else delete input.dataset.resume;input.click()}
async function refresh(notify=true){
 if(refreshBusy||!session)return;refreshBusy=true;
 try{
  const [f,c,s,u]=await Promise.all([api<{files:Entry[]}>('/api/files'),api<Capacity>('/api/storage'),api<{shares:Share[]}>('/api/shares'),api<{uploads:RemoteUpload[]}>('/api/uploads')]);
  if(!session)return;files=f.files;capacity=c;shares=s.shares;
  for(const remote of u.uploads){if(!jobs.some(j=>j.url&&new URL(j.url,location.origin).pathname.endsWith('/'+remote.id))){jobs.push({key:crypto.randomUUID(),name:remote.name,size:remote.size,offset:remote.offset,state:'waiting',url:remote.upload_url,speed:0,at:0,last:0})}}
  jobs=jobs.filter(j=>j.state!=='waiting'||u.uploads.some(x=>j.url&&new URL(j.url,location.origin).pathname.endsWith('/'+x.id)));
  render();
 }catch(e){if(notify)message(errorText(e),true)}finally{refreshBusy=false}
}
function render(){
 if(!document.querySelector('#view'))return;
 const title={files:'全部文件',uploads:'传输任务',shares:'我的分享'}[tab]||'全部文件';
 document.querySelector('#page-title')!.textContent=title;
 document.querySelector('#page-subtitle')!.textContent={files:'有序存放，自由分享。',uploads:'网络中断，也不用从头开始。',shares:'每一条链接，都由你掌控。'}[tab]||'';
 document.querySelector('#list-title')!.textContent=title;
 document.querySelectorAll<HTMLButtonElement>('[data-tab]').forEach(b=>{b.classList.toggle('active',b.dataset.tab===tab);b.setAttribute('aria-current',b.dataset.tab===tab?'page':'false')});
 document.querySelector('#file-count')!.textContent=String(files.length);
 document.querySelector('#upload-count')!.textContent=String(jobs.filter(j=>j.state!=='done').length);
 document.querySelector('#share-count')!.textContent=String(shares.length);
 (document.querySelector('#dropzone') as HTMLElement).hidden=tab==='shares';
 (document.querySelector('#search') as HTMLElement).hidden=tab!=='files';
 (document.querySelector('#sort') as HTMLElement).hidden=tab!=='files';
 renderCapacity();renderList();
}
function renderCapacity(){
 const host=document.querySelector('#capacity')!;host.replaceChildren();if(!capacity)return;
 const c=capacity;const primary=el('div','capacity-main');const top=el('div','capacity-top');top.append(el('span','muted','磁盘存储空间'),el('span','badge','实时'));
 const headline=el('div','capacity-number');headline.append(el('strong','',bytes(c.available)),el('span','','剩余可用'));
 const track=el('div','meter');const fill=el('div');fill.style.width=Math.min(100,c.total?c.used/c.total*100:0)+'%';track.append(fill);
 const detail=el('p','capacity-detail','已用 '+bytes(c.used)+' / 总容量 '+bytes(c.total));primary.append(top,headline,track,detail);
 const secondary=el('div','capacity-stats');
 for(const [label,value,help] of [['还能接受的新上传',c.upload_available,'已扣除系统保留及待传预留'],['网盘实际占用',c.files_bytes+c.partial_bytes,'含未完成上传已写入的部分'],['待传预留空间',c.pending_bytes,'取消任务后释放预留']] as [string,number,string][]){
  const item=el('div','capacity-stat');item.append(el('span','muted',label),el('strong','',bytes(value)),el('small','',help));secondary.append(item);
 }
 host.append(primary,secondary);
}
function empty(title:string,description:string){const box=el('div','empty');box.append(el('div','empty-symbol','◇'),el('h3','',title),el('p','muted',description));return box}
function renderList(){
 const host=document.querySelector('#view');if(!host)return;host.replaceChildren();
 if(tab==='files'){
  const filtered=files.filter(f=>f.name.toLocaleLowerCase().includes(search.toLocaleLowerCase())).sort((a,b)=>sort==='name'?a.name.localeCompare(b.name):sort==='size'?b.size-a.size:b.created_at.localeCompare(a.created_at));
  if(!filtered.length){host.append(empty(search?'没有匹配的文件':'你的文件空间，准备好了',search?'试试其他文件名。':'上传第一份文件，随后就能创建分享链接。'));return}
  const table=el('div','file-table');const head=el('div','file-row table-head');for(const text of ['文件名称','大小','上传时间','操作'])head.append(el('span','',text));table.append(head);
  for(const f of filtered){
   const row=el('div','file-row');const name=el('div','file-name');const symbol=el('span','file-symbol',f.name.split('.').pop()?.slice(0,4).toUpperCase()||'FILE');const text=el('strong','truncate',f.name);text.title=f.name;name.append(symbol,text);
   const actions=el('div','row-actions');const download=el('a','text-button','下载');download.href='/api/files/'+f.id+'/download';download.setAttribute('download','');
   actions.append(download,action('分享',()=>void shareFile(f),'text-button'),action('重命名',()=>void renameFile(f),'text-button'),action('删除',()=>void deleteFile(f),'text-button danger'));
   row.append(name,el('span','file-size',bytes(f.size)),el('span','file-date',date(f.created_at)),actions);table.append(row);
  }host.append(table);
 }else if(tab==='shares'){
  if(!shares.length){host.append(empty('还没有分享链接','在文件列表中点击「分享」，为文件创建一个下载入口。'));return}
  for(const s of shares){const card=el('article','share-row');const info=el('div','share-info');info.append(el('strong','truncate',s.file_name),el('p','muted',bytes(s.size)+' · '+(s.expires_at?(Date.parse(s.expires_at)<Date.now()?'已过期 · ':'到期 ')+date(s.expires_at):'永久有效')));
   const link=el('a','share-url truncate',absolute(s.url));link.href=absolute(s.url);link.target='_blank';link.rel='noopener noreferrer';info.append(link);
   const actions=el('div','row-actions');actions.append(action('复制分享',()=>void copy(s.url),'button secondary small'),action('复制直链',()=>void copy(s.download_url),'button secondary small'),action('撤销',()=>void revokeShare(s),'text-button danger'));card.append(info,actions);host.append(card)}
 }else{
  const summary=el('div','queue-summary');summary.append(el('span','muted','已完成 '+jobs.filter(j=>j.state==='done').length+' 项 · 同时上传最多 2 个文件'),action('清除已完成',()=>{jobs=jobs.filter(j=>j.state!=='done');render()},'text-button'));host.append(summary);
  if(!jobs.length){host.append(empty('暂无传输任务','选择文件开始上传。暂停的任务会保留服务器上的进度。'));return}
  for(const j of jobs){
   const row=el('article','upload-row');row.dataset.job=j.key;
   const top=el('div','upload-top');top.append(el('strong','truncate',j.name),el('span','upload-state '+j.state,labels[j.state]));
   const progress=el('progress');progress.max=j.size||1;progress.value=j.state==='done'?progress.max:j.offset;progress.setAttribute('aria-label',j.name+' 上传进度');
   const bottom=el('div','upload-bottom');const text=el('span','muted',bytes(j.offset)+' / '+bytes(j.size)+(j.state==='uploading'?' · '+bytes(j.speed)+'/s':''));
   const controls=el('div','row-actions');
   if(j.state==='uploading')controls.append(action('暂停',()=>void pause(j),'text-button'));
   if(j.state==='paused'||j.state==='error')controls.append(action(j.file?'继续上传':'选择原文件',()=>j.file?resume(j):pickFiles(j.key),'text-button'));
   if(j.state==='waiting')controls.append(action('选择原文件续传',()=>pickFiles(j.key),'text-button'));
   if(j.state!=='done')controls.append(action('取消',()=>void cancel(j),'text-button danger'));
   bottom.append(text,controls);row.append(top,progress,bottom);
   if(j.error)row.append(el('p','form-error',j.error));host.append(row);
  }
 }
}
async function renameFile(f:Entry){const name=await promptDialog('重命名文件','文件名称',f.name);if(name===null||name===f.name)return;try{await api('/api/files/'+f.id+'/rename','POST',{name});message('文件已重命名');await refresh()}catch(e){message(errorText(e),true)}}
async function deleteFile(f:Entry){if(!confirm('永久删除「'+f.name+'」？已有分享也将失效。'))return;try{await api('/api/files/'+f.id,'DELETE');message('文件已删除');await refresh()}catch(e){message(errorText(e),true)}}
async function shareFile(f:Entry){const hours=await promptDialog('创建分享链接',f.name,'0','创建分享',true,[['0','永久有效'],['24','1 天'],['168','7 天'],['720','30 天']]);if(hours===null)return;try{const s=await api<Share>('/api/files/'+f.id+'/shares','POST',{expires_in_hours:Number(hours)});await refresh();tab='shares';render();await copy(s.url)}catch(e){message(errorText(e),true)}}
async function revokeShare(s:Share){if(!confirm('撤销「'+s.file_name+'」的分享链接？'))return;try{await api('/api/shares/'+s.id,'DELETE');message('分享已撤销');await refresh()}catch(e){message(errorText(e),true)}}

function updateProgress(){if(renderPending)return;renderPending=true;setTimeout(()=>{
 renderPending=false;if(tab!=='uploads')return;
 for(const job of jobs){const row=document.querySelector<HTMLElement>('[data-job="'+job.key+'"]');if(!row)continue;
 const progress=row.querySelector('progress');if(progress){progress.max=job.size||1;progress.value=job.offset}
 const text=row.querySelector('.upload-bottom>.muted');if(text)text.textContent=bytes(job.offset)+' / '+bytes(job.size)+(job.state==='uploading'?' · '+bytes(job.speed)+'/s':'');
 }
},200)}
async function addFiles(selected:File[],resumeKey?:string){
 if(!selected.length)return;
 if(!crypto.subtle){message('请通过 HTTPS 或本机 localhost 打开网盘，以启用上传校验。',true);return}
 for(const file of selected){
  let j:Job|undefined=resumeKey?jobs.find(x=>x.key===resumeKey):undefined;
  if(j){if(file.name!==j.name||file.size!==j.size){message('请选择名称和大小均一致的原文件。',true);continue}
   if(!confirm('请确认这是「'+j.name+'」上传时的原文件。若内容已修改，请取消旧任务后重新上传。'))continue;
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
   onShouldRetry:(err,attempt)=>{const status=err.originalResponse?.getStatus()||0;return attempt<6&&(status===0||status===409||status===423||status===429||status===460||status>=500&&status!==507)},
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
async function pause(j:Job){j.generation=(j.generation||0)+1;j.state='paused';await j.upload?.abort();j.speed=0;render();pump()}
function resume(j:Job){j.error=undefined;if(!j.upload){pickFiles(j.key);return}j.state='queued';render();pump()}
async function cancel(j:Job){
 if(!confirm('取消「'+j.name+'」并删除已上传的部分？'))return;
 try{j.generation=(j.generation||0)+1;j.state='paused';await j.upload?.abort();const url=j.upload?.url||j.url;if(url){const r=await fetch(absolute(url),{method:'DELETE',headers:{'Tus-Resumable':'1.0.0','X-CSRF-Token':session?.csrf||''},credentials:'same-origin'});if(!r.ok&&r.status!==404)throw new Error('取消失败，请重试')}
 jobs=jobs.filter(x=>x!==j);message('上传任务已取消');await refresh(false);pump();
 }catch(e){j.state='error';j.error=errorText(e);render()}
}
async function shareView(token:string){
 root.innerHTML='<main class="public-layout"><a class="brand" href="/"><span class="brand-mark">S</span>solo<span class="brand-dot">.</span></a><section id="share-card" class="public-card" aria-live="polite"></section><p class="muted public-foot">私人存储，直接分享。</p></main>';
 const card=document.querySelector('#share-card')!;
 try{const s=await api<{file_name:string;size:number;expires_at:string|null;download_url:string}>('/api/public/shares/'+encodeURIComponent(token));
  card.append(el('span','eyebrow','SHARED WITH YOU'),el('div','public-file-icon','↓'),el('h1','public-name',s.file_name),el('p','muted',bytes(s.size)+' · '+(s.expires_at?'有效至 '+date(s.expires_at):'链接长期有效')));
  const link=el('a','button primary full','下载文件');link.href=absolute(s.download_url);link.setAttribute('download','');card.append(link,action('复制下载直链',()=>void copy(s.download_url),'button secondary full'),el('p','download-note','支持断点下载。将直链粘贴到下载器，可使用多连接下载。'));
 }catch(e){card.append(el('span','eyebrow','LINK UNAVAILABLE'),el('h1','','分享链接不可用'),el('p','muted',errorText(e)))}
}
async function boot(){
 const match=location.pathname.match(/^\/s\/([^/]+)\/?$/);if(match){await shareView(match[1]);return}
 try{session=await api<Session>('/api/session');await dashboard()}catch{loginView()}
}
void boot();
