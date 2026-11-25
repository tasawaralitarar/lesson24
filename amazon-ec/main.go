package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	_ "modernc.org/sqlite"
)

// 一般顧客向け（公開中の商品だけ）
type Product struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
	Image string `json:"image"` // 画像URL（画像なしは空文字）
	Stock int    `json:"stock"` // 第24回追加：現在の在庫数
}

// 第22回追加：管理者向け（在庫・非表示状態も含む）
type AdminProduct struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Price         int    `json:"price"`
	Stock         int    `json:"stock"`
	Image         string `json:"image"`
	ImageFilename string `json:"image_filename"`
	IsActive      int    `json:"is_active"`
}

// 第22回追加：管理者の登録・変更で受け取る値（省略した項目は変更しない）
type ProductInput struct {
	Name          *string `json:"name"`
	Price         *int    `json:"price"`
	Stock         *int    `json:"stock"`
	ImageFilename *string `json:"image_filename"`
}

const (
	uploadDir     = "./static/uploads"
	maxUploadSize = 5 << 20 // 5MB
)

// アップロード画像のファイル名（ランダム32文字 + 拡張子）だけを許可する
var imageNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|png|webp)$`)

type OrderRequest struct {
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

type OrderResponse struct {
	ID          int    `json:"id"`
	ProductName string `json:"product_name"`
	Price       int    `json:"price"`
	Quantity    int    `json:"quantity"`
	CreatedAt   string `json:"created_at"`
	// 第24回追加
	ShippingStatus string `json:"shipping_status"`
	PaymentStatus  string `json:"payment_status"`
}

type PaymentRequest struct {
	OrderIDs []int  `json:"order_ids"`
	Method   string `json:"method"`
}

var db *sql.DB

func main() {
	// Windowsでは .css が text/plain として配信されてデザインが効かないことがあるため、
	// 種類（Content-Type）をGo側で明示する。
	mime.AddExtensionType(".css", "text/css; charset=utf-8")
	mime.AddExtensionType(".js", "text/javascript; charset=utf-8")

	var err error
	db, err = sql.Open("sqlite", "./ec.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	initDB()

	// 第22回追加：初期管理者の設定  例) go run main.go --set-admin admin1
	if len(os.Args) >= 2 && os.Args[1] == "--set-admin" {
		if len(os.Args) != 3 {
			log.Fatal("使い方: go run main.go --set-admin 管理者ID")
		}
		if err := setAdmin(os.Args[2]); err != nil {
			log.Fatal("管理者設定失敗: ", err)
		}
		fmt.Println("管理者アカウントを設定しました:", strings.TrimSpace(os.Args[2]))
		return
	}

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatal("アップロード用フォルダ作成失敗: ", err)
	}

	// APIルーティング
	http.HandleFunc("/api/products", getProductsHandler)
	http.HandleFunc("/api/product", getProductDetailHandler)
	http.HandleFunc("/api/orders", ordersHandler)
	http.HandleFunc("/api/payments", createPaymentHandler)
	http.HandleFunc("/api/register", registerHandler)
	http.HandleFunc("/api/login", loginHandler)
	http.HandleFunc("/api/me", meHandler)
	http.HandleFunc("/api/logout", logoutHandler)

	// 第22回追加：管理者専用API（すべてGo側で管理者権限を確認する）
	http.HandleFunc("/api/admin/products", adminProductsHandler)
	http.HandleFunc("/api/admin/product", adminProductHandler)
	http.HandleFunc("/api/admin/upload", adminUploadHandler)

	// 第23回追加：配送管理
	http.HandleFunc("/api/admin/shipments", adminShipmentsHandler)
	http.HandleFunc("/api/admin/shipments/dispatch", adminDispatchHandler)

	// 第24回追加：キャンセル・返品・返金
	http.HandleFunc("/api/orders/cancel", cancelOrderHandler)
	http.HandleFunc("/api/orders/return", returnOrderHandler)
	http.HandleFunc("/api/admin/returns/approve", adminReturnApproveHandler)
	http.HandleFunc("/api/admin/returns/reject", adminReturnRejectHandler)
	http.HandleFunc("/api/admin/returns/refund-fail", adminRefundFailHandler)

	// 静的ファイルサーバー
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", noCache(fs))

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s ...\n", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}

func initDB() {
	createProductsTable := `
	CREATE TABLE IF NOT EXISTS products (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		price INTEGER NOT NULL
	);`
	_, err := db.Exec(createProductsTable)
	if err != nil {
		log.Fatal("Failed to create products table:", err)
	}

	createOrdersTable := `
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		product_id INTEGER NOT NULL,
		quantity INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	_, err = db.Exec(createOrdersTable)
	if err != nil {
		log.Fatal("Failed to create orders table:", err)
	}

	createPaymentsTable := `
	CREATE TABLE IF NOT EXISTS payments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
        order_id INTEGER NOT NULL,
		amount INTEGER NOT NULL,
		method TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	_, err = db.Exec(createPaymentsTable)
	if err != nil {
		log.Fatal("Failed to create payments table:", err)
	}

	// 既存データを保持し、顧客用のテーブルと列だけを追加する。
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY, user_id INTEGER NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(query); err != nil {
			log.Fatal("顧客テーブル作成失敗: ", err)
		}
	}
	var userIDColumns int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name = 'user_id'").Scan(&userIDColumns); err != nil {
		log.Fatal(err)
	}
	if userIDColumns == 0 {
		if _, err := db.Exec("ALTER TABLE orders ADD COLUMN user_id INTEGER"); err != nil {
			log.Fatal("注文テーブル移行失敗: ", err)
		}
	}
	// 古いpaymentsテーブルを自動削除・変更しない。
	var columnCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('payments') WHERE name = 'order_id'").Scan(&columnCount); err != nil || columnCount != 1 {
		log.Fatal("paymentsテーブルにorder_idがありません。ec.dbをバックアップし、移行手順を確認してください")
	}
	// 第22回追加：既存データを残したまま不足列だけを追加する（ec.dbは削除しない）
	for _, m := range []struct{ table, column, def string }{
		{"products", "stock", "INTEGER NOT NULL DEFAULT 0"},
		{"products", "image_filename", "TEXT"},
		{"products", "is_active", "INTEGER NOT NULL DEFAULT 1"},
		{"users", "is_admin", "INTEGER NOT NULL DEFAULT 0"},
		{"orders", "shipping_status", "TEXT NOT NULL DEFAULT '受付'"}, // 第23回
	} {
		addColumnIfMissing(m.table, m.column, m.def)
	}

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM products").Scan(&count)
	if err == nil && count == 0 {
		insertQuery := `INSERT INTO products (name, price) VALUES 
			('ノートパソコン', 120000),
			('ワイヤレスマウス', 3500),
			('USBメモリ 64GB', 1200);`
		_, err = db.Exec(insertQuery)
		if err != nil {
			log.Println("Failed to insert initial products:", err)
		} else {
			log.Println("Initial products inserted successfully.")
		}
	}
}

func getProductsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 一般顧客には公開中（is_active=1）の商品だけを返す
	rows, err := db.Query("SELECT id, name, price, stock, image_filename FROM products WHERE is_active = 1 ORDER BY id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var p Product
		var img sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.Stock, &img); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.Image = imageURL(img)
		products = append(products, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(products)
}

func getProductDetailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "Missing id parameter", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		http.Error(w, "Invalid id parameter", http.StatusBadRequest)
		return
	}

	var p Product
	var img sql.NullString
	err = db.QueryRow("SELECT id, name, price, stock, image_filename FROM products WHERE id = ? AND is_active = 1", id).Scan(&p.ID, &p.Name, &p.Price, &p.Stock, &img)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Product not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	p.Image = imageURL(img)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		createOrderHandler(w, r)
	case http.MethodGet:
		getOrdersHandler(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	// 第24回: ログイン必須・同一Originチェック。
	// SQLiteは同時接続数1のため、db.Begin() の前にログインユーザーIDを取得する。
	userID, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインが必要です", http.StatusUnauthorized)
		return
	}
	if !sameOrigin(w, r) {
		return
	}

	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	if req.ProductID <= 0 || req.Quantity <= 0 {
		http.Error(w, "Invalid product_id or quantity", http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// 有効商品かつ在庫が足りる場合だけ在庫を減らす（1つのUPDATEで原子的に処理）。
	res, err := tx.Exec("UPDATE products SET stock = stock - ? WHERE id = ? AND is_active = 1 AND stock >= ?", req.Quantity, req.ProductID, req.Quantity)
	if err != nil {
		http.Error(w, "在庫更新失敗", http.StatusInternalServerError)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		http.Error(w, "在庫更新確認失敗", http.StatusInternalServerError)
		return
	}
	if n != 1 {
		http.Error(w, "在庫が不足しているか、商品が公開されていません", http.StatusConflict)
		return
	}

	result, err := tx.Exec("INSERT INTO orders (product_id, quantity, user_id) VALUES (?, ?, ?)", req.ProductID, req.Quantity, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		http.Error(w, "注文ID取得失敗", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "注文確定失敗", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "message": "注文しました"})
}

func getOrdersHandler(w http.ResponseWriter, r *http.Request) {
	// 第24回: 注文履歴は本人のものだけ（ログイン必須）
	userID, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインが必要です", http.StatusUnauthorized)
		return
	}
	rows, err := db.Query(`SELECT orders.id, products.name, products.price, orders.quantity, orders.created_at, orders.shipping_status,
 COALESCE((SELECT pay.status FROM payments pay WHERE pay.order_id = orders.id ORDER BY pay.id DESC LIMIT 1), 'unpaid')
 FROM orders JOIN products ON orders.product_id = products.id
 WHERE orders.user_id = ? ORDER BY orders.id DESC`, userID)
	if err != nil {
		http.Error(w, "注文履歴取得失敗", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	orders := []OrderResponse{}
	for rows.Next() {
		var o OrderResponse
		if err := rows.Scan(&o.ID, &o.ProductName, &o.Price, &o.Quantity, &o.CreatedAt, &o.ShippingStatus, &o.PaymentStatus); err != nil {
			http.Error(w, "注文履歴取得失敗", http.StatusInternalServerError)
			return
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "注文履歴取得失敗", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orders)
}

func createPaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	// 第24回: ログイン必須・同一Originチェック
	userID, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインが必要です", http.StatusUnauthorized)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	var req PaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid input", 400)
		return
	}
	if len(req.OrderIDs) == 0 || (req.Method != "credit" && req.Method != "bank" && req.Method != "cod") {
		http.Error(w, "Invalid payment parameters", 400)
		return
	}
	seen := map[int]bool{}
	for _, id := range req.OrderIDs {
		if id <= 0 || seen[id] {
			http.Error(w, "Invalid order IDs", 400)
			return
		}
		seen[id] = true
	}
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", 500)
		return
	}
	defer tx.Rollback()
	for _, id := range req.OrderIDs {
		var amount int64
		var shippingStatus string
		// 第24回: 本人の注文だけを対象にする（他人の注文は「見つからない」扱い）
		err := tx.QueryRow(`SELECT CAST(products.price AS INTEGER) * CAST(orders.quantity AS INTEGER), orders.shipping_status
   FROM orders JOIN products ON orders.product_id = products.id WHERE orders.id = ? AND orders.user_id = ?`, id, userID).Scan(&amount, &shippingStatus)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Order not found", 404)
			return
		}
		if err != nil || amount <= 0 {
			http.Error(w, "Invalid order amount", 400)
			return
		}
		// キャンセル済み・発送済みなどの注文は決済できない
		if shippingStatus != "受付" {
			http.Error(w, "この注文は決済できません", 409)
			return
		}
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM payments WHERE order_id = ?", id).Scan(&count); err != nil {
			http.Error(w, "Payment check failed", 500)
			return
		}
		if count != 0 {
			http.Error(w, "Already paid", 409)
			return
		}
		if _, err := tx.Exec("INSERT INTO payments (order_id, amount, method, status) VALUES (?, ?, ?, 'paid')", id, amount, req.Method); err != nil {
			http.Error(w, "Payment insert failed", 500)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "Payment commit failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"message": "決済が完了しました", "status": "paid"})
}

// Cookieからセッションを検証する。ログインしていない場合はfalse。
func currentUserID(r *http.Request) (int64, bool) {
	cookie, err := r.Cookie("session_id")
	if err != nil || cookie.Value == "" {
		return 0, false
	}
	var id int64
	err = db.QueryRow("SELECT user_id FROM sessions WHERE token = ?", cookie.Value).Scan(&id)
	return id, err == nil
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 8 || len([]byte(req.Password)) > 72 {
		http.Error(w, "ユーザー名と8～72バイトのパスワードを入力してください", 400)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "登録処理失敗", 500)
		return
	}
	_, err = db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", req.Username, string(hash))
	if err != nil {
		http.Error(w, "ユーザー名が使用できません", 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"message": "会員登録しました"})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	var id int64
	var hash string
	err := db.QueryRow("SELECT id, password_hash FROM users WHERE username = ?", strings.TrimSpace(req.Username)).Scan(&id, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		http.Error(w, "ユーザー名またはパスワードが違います", 401)
		return
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		http.Error(w, "セッション作成失敗", 500)
		return
	}
	token := hex.EncodeToString(random)
	if _, err := db.Exec("INSERT INTO sessions (token, user_id) VALUES (?, ?)", token, id); err != nil {
		http.Error(w, "セッション保存失敗", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session_id", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "ログインしました"})
}

func meHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインしていません", 401)
		return
	}
	var username string
	var isAdmin int
	if err := db.QueryRow("SELECT username, is_admin FROM users WHERE id = ?", id).Scan(&username, &isAdmin); err != nil {
		http.Error(w, "ユーザーが見つかりません", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "username": username, "is_admin": isAdmin == 1})
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		db.Exec("DELETE FROM sessions WHERE token = ?", cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "session_id", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "ログアウトしました"})
}

// =====================================================
// 第22回追加：商品管理機能（管理者専用）
// =====================================================

// 列がなければ追加する（既存データはそのまま残る）
func addColumnIfMissing(table, column, definition string) {
	var n int
	q := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", table)
	if err := db.QueryRow(q, column).Scan(&n); err != nil {
		log.Fatal("列確認失敗: ", err)
	}
	if n == 0 {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
			log.Fatalf("%s.%s の追加に失敗: %v", table, column, err)
		}
		log.Printf("移行: %s.%s を追加しました", table, column)
	}
}

func imageURL(name sql.NullString) string {
	if name.Valid && imageNameRe.MatchString(name.String) {
		return "/uploads/" + name.String
	}
	return ""
}

// ログイン中かつ管理者（users.is_admin=1）か確認する。
// 管理画面を隠すだけでは不十分なので、すべての管理APIの先頭で呼ぶ。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインしてください", http.StatusUnauthorized)
		return false
	}
	var isAdmin int
	if err := db.QueryRow("SELECT is_admin FROM users WHERE id = ?", id).Scan(&isAdmin); err != nil || isAdmin != 1 {
		http.Error(w, "管理者権限がありません", http.StatusForbidden)
		return false
	}
	return true
}

// 初期管理者の設定（パスワードは画面に表示されず、ソースコードにも書かない）
func setAdmin(username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("管理者IDを指定してください")
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("パスワード入力にはターミナルで実行してください")
	}
	fmt.Print("管理者パスワード（8～72バイト／入力しても表示されません）: ")
	pw1, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Print("確認のためもう一度入力してください: ")
	pw2, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return err
	}
	if !bytes.Equal(pw1, pw2) {
		return errors.New("パスワードが一致しません")
	}
	if len(pw1) < 8 || len(pw1) > 72 {
		return errors.New("パスワードは8～72バイトにしてください")
	}
	hash, err := bcrypt.GenerateFromPassword(pw1, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := db.Exec("UPDATE users SET password_hash = ?, is_admin = 1 WHERE username = ?", string(hash), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := db.Exec("INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 1)", username, string(hash)); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func validateProduct(name string, price, stock int, imageFilename string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return errors.New("商品名は1～100文字で入力してください")
	}
	if price < 1 || price > 10000000 {
		return errors.New("価格は1～10,000,000円で入力してください")
	}
	if stock < 0 || stock > 1000000 {
		return errors.New("在庫数量は0～1,000,000で入力してください")
	}
	if imageFilename != "" && !imageNameRe.MatchString(imageFilename) {
		return errors.New("画像ファイル名が不正です")
	}
	return nil
}

// GET /api/admin/products（全商品一覧） / POST /api/admin/products（商品登録）
func adminProductsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query("SELECT id, name, price, stock, image_filename, is_active FROM products ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		list := []AdminProduct{}
		for rows.Next() {
			var p AdminProduct
			var img sql.NullString
			if err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.Stock, &img, &p.IsActive); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			p.Image = imageURL(img)
			if p.Image != "" {
				p.ImageFilename = img.String
			}
			list = append(list, p)
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		var in ProductInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == nil || in.Price == nil {
			http.Error(w, "商品名と価格を入力してください", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(*in.Name)
		stock := 0
		if in.Stock != nil {
			stock = *in.Stock
		}
		imageFilename := ""
		if in.ImageFilename != nil {
			imageFilename = *in.ImageFilename
		}
		if err := validateProduct(name, *in.Price, stock, imageFilename); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var img interface{}
		if imageFilename != "" {
			img = imageFilename
		}
		res, err := db.Exec("INSERT INTO products (name, price, stock, image_filename, is_active) VALUES (?, ?, ?, ?, 1)", name, *in.Price, stock, img)
		if err != nil {
			http.Error(w, "商品登録に失敗しました", http.StatusInternalServerError)
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "message": "商品を登録しました"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// PUT /api/admin/product?id=番号（商品変更） / DELETE /api/admin/product?id=番号（論理削除＝非表示）
func adminProductHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id <= 0 {
		http.Error(w, "Invalid id parameter", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		// 物理削除はしない。is_active=0 にするだけなので、過去の注文・決済は壊れない。
		res, err := db.Exec("UPDATE products SET is_active = 0 WHERE id = ?", id)
		if err != nil {
			http.Error(w, "非表示に失敗しました", http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "Product not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "商品を非表示にしました"})

	case http.MethodPut:
		var in ProductInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		// 現在の値を読み込み、送られてきた項目だけ上書きする
		var name string
		var price, stock, isActive int
		var img sql.NullString
		err := db.QueryRow("SELECT name, price, stock, image_filename, is_active FROM products WHERE id = ?", id).Scan(&name, &price, &stock, &img, &isActive)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Product not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
		}
		if in.Price != nil {
			price = *in.Price
		}
		if in.Stock != nil {
			stock = *in.Stock
		}
		if in.ImageFilename != nil {
			if *in.ImageFilename == "" {
				img = sql.NullString{}
			} else {
				img = sql.NullString{String: *in.ImageFilename, Valid: true}
			}
		}
		if err := validateProduct(name, price, stock, img.String); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var imgValue interface{}
		if img.Valid {
			imgValue = img.String
		}
		if _, err := db.Exec("UPDATE products SET name = ?, price = ?, stock = ?, image_filename = ?, is_active = ? WHERE id = ?",
			name, price, stock, imgValue, isActive, id); err != nil {
			http.Error(w, "商品変更に失敗しました", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "商品を変更しました"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// POST /api/admin/upload（商品画像アップロード）
// JPEG・PNG・WebPのみ、5MBまで。ファイル名はサーバー側でランダムに決める。
func adminUploadHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+(1<<20))
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "画像が大きすぎます（5MBまで）", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "画像ファイルを選択してください", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size <= 0 || header.Size > maxUploadSize {
		http.Error(w, "画像は5MB以下にしてください", http.StatusBadRequest)
		return
	}

	// 拡張子ではなく、ファイルの中身で形式を判定する
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		http.Error(w, "画像を読み込めません", http.StatusBadRequest)
		return
	}
	ext := ""
	switch http.DetectContentType(head[:n]) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	default:
		http.Error(w, "JPEG・PNG・WebPの画像だけアップロードできます", http.StatusBadRequest)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "画像を読み込めません", http.StatusInternalServerError)
		return
	}

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		http.Error(w, "ファイル名作成失敗", http.StatusInternalServerError)
		return
	}
	filename := hex.EncodeToString(random) + ext
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		http.Error(w, "保存先フォルダを作成できません", http.StatusInternalServerError)
		return
	}
	dstPath := filepath.Join(uploadDir, filename)
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		http.Error(w, "画像を保存できません", http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(dstPath)
		http.Error(w, "画像を保存できません", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"filename": filename, "url": "/uploads/" + filename})
}

// ===== 第23回 配送管理 =====
type ShipmentResponse struct {
	ID             int64  `json:"id"`
	ProductName    string `json:"product_name"`
	Quantity       int    `json:"quantity"`
	CreatedAt      string `json:"created_at"`
	ShippingStatus string `json:"shipping_status"`
	PaymentStatus  string `json:"payment_status"`
}

func adminShipmentsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	rows, err := db.Query(`SELECT o.id,p.name,o.quantity,o.created_at,o.shipping_status,
 COALESCE((SELECT pay.status FROM payments pay WHERE pay.order_id=o.id ORDER BY pay.id DESC LIMIT 1), 'unpaid')
 FROM orders o JOIN products p ON p.id=o.product_id ORDER BY o.id DESC`)
	if err != nil {
		http.Error(w, "配送一覧取得失敗", 500)
		return
	}
	defer rows.Close()
	items := []ShipmentResponse{}
	for rows.Next() {
		var item ShipmentResponse
		if err := rows.Scan(&item.ID, &item.ProductName, &item.Quantity, &item.CreatedAt, &item.ShippingStatus, &item.PaymentStatus); err != nil {
			http.Error(w, "配送一覧取得失敗", 500)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "配送一覧取得失敗", 500)
		return
	}
	writeJSON(w, 200, items)
}
func adminDispatchHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.OrderID <= 0 {
		http.Error(w, "有効なorder_idを指定してください", 400)
		return
	}
	// 単一UPDATEで決済済み・未発送・存在する注文だけを原子的に更新する。
	result, err := db.Exec(`UPDATE orders SET shipping_status='発送済み'
 WHERE id=? AND shipping_status='受付'
 AND EXISTS(SELECT 1 FROM payments WHERE payments.order_id=orders.id AND payments.status='paid')`, req.OrderID)
	if err != nil {
		http.Error(w, "発送更新失敗", 500)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "発送更新確認失敗", 500)
		return
	}
	if n != 1 {
		http.Error(w, "注文が存在しない、未決済、または発送済みです", 409)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": req.OrderID, "shipping_status": "発送済み"})
}

// ===== 第24回 キャンセル・返品・模擬返金 =====

// 同一Originチェック（Cookieを使うPOST/PUT/DELETE向け）
func sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		http.Error(w, "Originヘッダーが必要です", 403)
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "Originが一致しません", 403)
		return false
	}
	return true
}

// order_id だけを受け取る共通リクエスト
func readOrderID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.OrderID <= 0 {
		http.Error(w, "有効なorder_idを指定してください", 400)
		return 0, false
	}
	return req.OrderID, true
}

// 顧客向けキャンセル（未発送の本人の注文のみ）
func cancelOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	// db.Begin() の前にユーザーIDを取得する（SQLite同時接続1のデッドロック防止）
	userID, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインが必要です", http.StatusUnauthorized)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	orderID, ok := readOrderID(w, r)
	if !ok {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", 500)
		return
	}
	defer tx.Rollback()

	var productID int64
	var quantity int
	var status string
	err = tx.QueryRow("SELECT product_id, quantity, shipping_status FROM orders WHERE id = ? AND user_id = ?", orderID, userID).Scan(&productID, &quantity, &status)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "注文が見つかりません", 404)
		return
	}
	if err != nil {
		http.Error(w, "注文確認失敗", 500)
		return
	}
	if status != "受付" {
		http.Error(w, "未発送の注文のみキャンセルできます", 409)
		return
	}
	res, err := tx.Exec("UPDATE orders SET shipping_status = 'キャンセル済み' WHERE id = ? AND user_id = ? AND shipping_status = '受付'", orderID, userID)
	if err != nil {
		http.Error(w, "キャンセル失敗", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "この注文はキャンセルできません", 409)
		return
	}
	// 在庫を戻す
	if _, err := tx.Exec("UPDATE products SET stock = stock + ? WHERE id = ?", quantity, productID); err != nil {
		http.Error(w, "在庫戻し失敗", 500)
		return
	}
	// 決済済みなら模擬返金（paid → refunded）
	if _, err := tx.Exec("UPDATE payments SET status = 'refunded' WHERE order_id = ? AND status = 'paid'", orderID); err != nil {
		http.Error(w, "返金処理失敗", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "キャンセル確定失敗", 500)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": orderID, "shipping_status": "キャンセル済み"})
}

// 顧客向け返品申請（発送済み・決済済みの本人の注文のみ）
func returnOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	userID, ok := currentUserID(r)
	if !ok {
		http.Error(w, "ログインが必要です", http.StatusUnauthorized)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	orderID, ok := readOrderID(w, r)
	if !ok {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", 500)
		return
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow("SELECT shipping_status FROM orders WHERE id = ? AND user_id = ?", orderID, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "注文が見つかりません", 404)
		return
	}
	if err != nil {
		http.Error(w, "注文確認失敗", 500)
		return
	}
	res, err := tx.Exec(`UPDATE orders SET shipping_status = '返品申請中'
 WHERE id = ? AND user_id = ? AND shipping_status = '発送済み'
 AND (SELECT pay.status FROM payments pay WHERE pay.order_id = orders.id ORDER BY pay.id DESC LIMIT 1) = 'paid'`, orderID, userID)
	if err != nil {
		http.Error(w, "返品申請失敗", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "発送済みかつ決済済みの注文のみ返品申請できます", 409)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "返品申請確定失敗", 500)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": orderID, "shipping_status": "返品申請中"})
}

// 管理者：返品承認・模擬返金・在庫戻し
func adminReturnApproveHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	orderID, ok := readOrderID(w, r)
	if !ok {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", 500)
		return
	}
	defer tx.Rollback()

	var productID int64
	var quantity int
	var status string
	err = tx.QueryRow("SELECT product_id, quantity, shipping_status FROM orders WHERE id = ?", orderID).Scan(&productID, &quantity, &status)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "注文が見つかりません", 404)
		return
	}
	if err != nil {
		http.Error(w, "注文確認失敗", 500)
		return
	}
	if status != "返品申請中" {
		http.Error(w, "返品申請中の注文のみ承認できます", 409)
		return
	}
	// 決済記録の最新レコードが paid であることを厳密に再確認する
	var paymentID int64
	var paymentStatus string
	err = tx.QueryRow("SELECT id, status FROM payments WHERE order_id = ? ORDER BY id DESC LIMIT 1", orderID).Scan(&paymentID, &paymentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "決済記録がないため返金できません", 409)
		return
	}
	if err != nil {
		http.Error(w, "決済確認失敗", 500)
		return
	}
	if paymentStatus != "paid" {
		http.Error(w, "決済済みではないため返金できません", 409)
		return
	}
	res, err := tx.Exec("UPDATE orders SET shipping_status = '返品済み' WHERE id = ? AND shipping_status = '返品申請中'", orderID)
	if err != nil {
		http.Error(w, "返品承認失敗", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "返品承認できません", 409)
		return
	}
	if _, err := tx.Exec("UPDATE products SET stock = stock + ? WHERE id = ?", quantity, productID); err != nil {
		http.Error(w, "在庫戻し失敗", 500)
		return
	}
	res, err = tx.Exec("UPDATE payments SET status = 'refunded' WHERE id = ? AND status = 'paid'", paymentID)
	if err != nil {
		http.Error(w, "返金処理失敗", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "返金処理できません（二重返金の可能性）", 409)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "返品承認確定失敗", 500)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": orderID, "shipping_status": "返品済み", "payment_status": "refunded"})
}

// 管理者：返品却下（在庫・決済記録は変更しない）
func adminReturnRejectHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	orderID, ok := readOrderID(w, r)
	if !ok {
		return
	}
	res, err := db.Exec("UPDATE orders SET shipping_status = '返品却下' WHERE id = ? AND shipping_status = '返品申請中'", orderID)
	if err != nil {
		http.Error(w, "返品却下失敗", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "返品申請中の注文のみ却下できます", 409)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": orderID, "shipping_status": "返品却下"})
}

// 管理者：返金失敗の記録（payments.status を refund_failed にする）
func adminRefundFailHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if !sameOrigin(w, r) {
		return
	}
	orderID, ok := readOrderID(w, r)
	if !ok {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Transaction error", 500)
		return
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow("SELECT shipping_status FROM orders WHERE id = ?", orderID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "注文が見つかりません", 404)
		return
	}
	if err != nil {
		http.Error(w, "注文確認失敗", 500)
		return
	}
	if status != "返品申請中" {
		http.Error(w, "返品申請中の注文のみ記録できます", 409)
		return
	}
	var paymentID int64
	var paymentStatus string
	err = tx.QueryRow("SELECT id, status FROM payments WHERE order_id = ? ORDER BY id DESC LIMIT 1", orderID).Scan(&paymentID, &paymentStatus)
	if err != nil || paymentStatus != "paid" {
		http.Error(w, "決済済みの注文のみ返金失敗を記録できます", 409)
		return
	}
	res, err := tx.Exec("UPDATE payments SET status = 'refund_failed' WHERE id = ? AND status = 'paid'", paymentID)
	if err != nil {
		http.Error(w, "返金失敗の記録に失敗しました", 500)
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "返金失敗を記録できません", 409)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "確定失敗", 500)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"order_id": orderID, "payment_status": "refund_failed"})
}

// 授業・開発用：ブラウザが古いCSS/JSを使い続けないようにする
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
