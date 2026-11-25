// 決済状態の表示名（第24回追加）
const PAYMENT_LABELS = {
    unpaid: "未決済",
    paid: "決済済み",
    refunded: "返金済み",
    refund_failed: "返金失敗"
};

// 顧客向けのPOST操作（キャンセル・返品申請）
async function postOrderAction(path, orderId) {
    const response = await fetch(path, {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({order_id: orderId})
    });
    if (!response.ok) {
        const text = (await response.text()).trim();
        throw new Error(text || "処理に失敗しました。");
    }
    return response.json();
}

function createActionButton(label, handler) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = label;
    btn.addEventListener("click", handler);
    return btn;
}

async function loadOrders() {
    const tbody = document.getElementById("order-items");
    try {
        const me = await fetch("/api/me");
        if (!me.ok) {
            tbody.innerHTML = '<tr><td colspan="7">自分の注文履歴を見るにはログインしてください。<a href="login.html">ログイン</a></td></tr>';
            return;
        }
        const response = await fetch("/api/orders");
        if (!response.ok) throw new Error("注文履歴の取得に失敗しました。");
        const orders = await response.json();
        tbody.textContent = "";
        if (!orders.length) {
            tbody.innerHTML = '<tr><td colspan="7">自分の注文履歴はありません。</td></tr>';
            return;
        }
        for (const item of orders) {
            const row = document.createElement("tr");
            const paymentLabel = PAYMENT_LABELS[item.payment_status] || item.payment_status;
            const values = [item.product_name, `¥${item.price.toLocaleString()}`, String(item.quantity), `¥${(item.price * item.quantity).toLocaleString()}`, paymentLabel, item.shipping_status];
            for (const value of values) {
                const cell = document.createElement("td");
                cell.textContent = value;
                row.appendChild(cell);
            }

            // 操作ボタン（第24回追加）
            const actionCell = document.createElement("td");
            const isReceived = item.shipping_status === "受付";
            if (isReceived && item.payment_status === "unpaid") {
                actionCell.appendChild(createActionButton("決済する", () => {
                    window.location.href = `payment.html?orders=${item.id}`;
                }));
            }
            if (isReceived) {
                actionCell.appendChild(createActionButton("キャンセル", async (e) => {
                    if (!confirm("この注文をキャンセルしますか？")) return;
                    e.target.disabled = true;
                    try {
                        await postOrderAction("/api/orders/cancel", item.id);
                        alert("キャンセルしました。");
                    } catch (error) {
                        alert(error.message);
                    }
                    await loadOrders();
                }));
            }
            if (item.shipping_status === "発送済み" && item.payment_status === "paid") {
                actionCell.appendChild(createActionButton("返品申請", async (e) => {
                    if (!confirm("この注文の返品を申請しますか？")) return;
                    e.target.disabled = true;
                    try {
                        await postOrderAction("/api/orders/return", item.id);
                        alert("返品を申請しました。");
                    } catch (error) {
                        alert(error.message);
                    }
                    await loadOrders();
                }));
            }
            if (!actionCell.hasChildNodes()) actionCell.textContent = "－";
            row.appendChild(actionCell);

            tbody.appendChild(row);
        }
    } catch (error) {
        console.error(error);
        tbody.innerHTML = '<tr><td colspan="7">注文履歴の取得に失敗しました。</td></tr>';
    }
}

document.addEventListener("DOMContentLoaded", loadOrders);
