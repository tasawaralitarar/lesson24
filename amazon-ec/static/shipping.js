"use strict";
const byId = id => document.getElementById(id);
async function api(path, options={}) {
 const res=await fetch(path,{credentials:"same-origin",...options});
 if(!res.ok) throw new Error((await res.text()).trim()||`HTTP ${res.status}`);
 return res.json();
}
const PAYMENT_LABELS={unpaid:"未決済",paid:"決済済み",refunded:"返金済み",refund_failed:"返金失敗"};
async function postAdmin(path,orderId){
 return api(path,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({order_id:orderId})});
}
function addAction(td,label,path,order,confirmText,doneText){
 const btn=document.createElement("button");btn.type="button";btn.textContent=label;
 btn.addEventListener("click",async()=>{
  if(!confirm(confirmText))return;
  btn.disabled=true;
  try{await postAdmin(path,order.id);await load();byId("message").textContent=doneText;}
  catch(e){byId("message").textContent=e.message;await load().catch(()=>{});}
 });td.appendChild(btn);
}
async function load(){
 const orders=await api("/api/admin/shipments");const tbody=byId("shipments");tbody.replaceChildren();
 for(const order of orders){
  const tr=document.createElement("tr");
  for(const value of [order.id,order.product_name,order.quantity,order.created_at,PAYMENT_LABELS[order.payment_status]||order.payment_status,order.shipping_status]){
   const td=document.createElement("td");td.textContent=String(value);tr.appendChild(td);
  }
  const td=document.createElement("td");
  if(order.payment_status==="paid"&&order.shipping_status==="受付"){
   const btn=document.createElement("button");btn.type="button";btn.textContent="発送済みにする";
   btn.addEventListener("click",async()=>{
    if(!confirm(`注文 ${order.id} を発送済みにしますか？`))return;
    btn.disabled=true;
    try{await api("/api/admin/shipments/dispatch",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({order_id:order.id})});await load();byId("message").textContent="発送状態を更新しました。";}
    catch(e){byId("message").textContent=e.message;await load().catch(()=>{});}
   });td.appendChild(btn);
  }else if(order.shipping_status==="返品申請中"){
   // 第24回追加：返品承認・却下・返金失敗記録
   addAction(td,"返品承認・返金","/api/admin/returns/approve",order,`注文 ${order.id} の返品を承認して模擬返金しますか？`,"返品を承認し、返金済みにしました。");
   addAction(td,"返品却下","/api/admin/returns/reject",order,`注文 ${order.id} の返品を却下しますか？`,"返品を却下しました。");
   addAction(td,"返金失敗記録","/api/admin/returns/refund-fail",order,`注文 ${order.id} の返金失敗を記録しますか？`,"返金失敗を記録しました。");
  }else{td.textContent="－";}
  tr.appendChild(td);tbody.appendChild(tr);
 }
}
async function init(){try{const me=await api("/api/me");if(!me.is_admin)throw new Error("管理者権限がありません");byId("panel").hidden=false;byId("message").textContent=`管理者：${me.username}`;await load();}catch(e){byId("message").textContent=e.message;}}
byId("reload").addEventListener("click",()=>load().catch(e=>byId("message").textContent=e.message));
init();
