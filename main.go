package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var defaultCategories = []string{
	"Еда",
	"Транспорт",
	"Кафе",
	"Подписки",
	"Одежда",
	"Дом",
	"Здоровье",
	"Подарки",
	"Развлечения",
	"Другое",
}

type userState struct {
	Action     string
	CategoryID int64
	ExpenseID  int64
	Amount     float64
	Note       string
}

const (
	actionWaitAmount      = "wait_amount"
	actionWaitCustomTitle = "wait_custom_title"
	actionWaitEditAmount  = "wait_edit_amount"
	actionWaitNote        = "wait_note"
	actionWaitQuickCat    = "wait_quick_category"
)

func main() {
	_ = godotenv.Load()

	botToken := os.Getenv("BOT_TOKEN")
	dbURL := os.Getenv("DATABASE_URL")
	if botToken == "" || dbURL == "" {
		log.Fatal("Set BOT_TOKEN and DATABASE_URL in environment")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("db open error: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("db ping error: %v", err)
	}
	if err := runMigrations(db); err != nil {
		log.Fatalf("migration error: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("bot init error: %v", err)
	}

	log.Printf("Bot started as @%s", bot.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	states := map[int64]userState{}

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, db, states, update.Message)
		}
		if update.CallbackQuery != nil {
			handleCallback(bot, db, states, update.CallbackQuery)
		}
	}
}

func runMigrations(db *sql.DB) error {
	query := `
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    tg_user_id BIGINT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS categories (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, name)
);

CREATE TABLE IF NOT EXISTS expenses (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category_id BIGINT NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    amount NUMERIC(12,2) NOT NULL CHECK (amount > 0),
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
	_, err := db.Exec(query)
	return err
}

func handleMessage(bot *tgbotapi.BotAPI, db *sql.DB, states map[int64]userState, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	tgUserID := msg.From.ID

	userID, err := ensureUser(db, tgUserID)
	if err != nil {
		sendText(bot, chatID, "Ошибка базы данных.")
		return
	}
	if err := ensureDefaultCategories(db, userID); err != nil {
		sendText(bot, chatID, "Ошибка инициализации категорий.")
		return
	}

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			delete(states, tgUserID)
			sendMainMenu(bot, chatID, "Привет! Это простой бот учета расходов. Все действия через кнопки ниже.")
		case "menu":
			delete(states, tgUserID)
			sendMainMenu(bot, chatID, "Главное меню")
		default:
			sendText(bot, chatID, "Доступные команды: /start, /menu")
		}
		return
	}

	state, ok := states[tgUserID]
	if !ok {
		amount, note, parsed := parseQuickExpenseInput(msg.Text)
		if parsed {
			if len([]rune(note)) > 120 {
				sendText(bot, chatID, "Комментарий слишком длинный. До 120 символов.")
				return
			}
			states[tgUserID] = userState{
				Action: actionWaitQuickCat,
				Amount: amount,
				Note:   note,
			}
			if err := sendCategoryKeyboard(bot, db, chatID, userID, "Выбери категорию для быстрого расхода:", "quickcat"); err != nil {
				sendText(bot, chatID, "Не удалось загрузить категории.")
			}
			return
		}
		sendMainMenu(bot, chatID, "Используй кнопки ниже.")
		return
	}

	switch state.Action {
	case actionWaitCustomTitle:
		name := strings.TrimSpace(msg.Text)
		if name == "" {
			sendText(bot, chatID, "Название категории не может быть пустым. Введи еще раз.")
			return
		}
		if len([]rune(name)) > 40 {
			sendText(bot, chatID, "Слишком длинно. До 40 символов.")
			return
		}
		if err := addCategory(db, userID, name); err != nil {
			if strings.Contains(err.Error(), "duplicate") {
				sendText(bot, chatID, "Такая категория уже есть.")
			} else {
				sendText(bot, chatID, "Не удалось сохранить категорию.")
			}
			return
		}
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, fmt.Sprintf("Категория \"%s\" добавлена.", name))
	case actionWaitAmount:
		amountText := strings.ReplaceAll(strings.TrimSpace(msg.Text), ",", ".")
		amount, err := strconv.ParseFloat(amountText, 64)
		if err != nil || amount <= 0 {
			sendText(bot, chatID, "Введи сумму числом, например: 350.50")
			return
		}
		states[tgUserID] = userState{
			Action:     actionWaitNote,
			CategoryID: state.CategoryID,
			Amount:     amount,
		}
		sendText(bot, chatID, "Теперь введи комментарий к расходу (например: \"кофе в центре\"). Можно отправить '-' чтобы пропустить.")
	case actionWaitNote:
		note := strings.TrimSpace(msg.Text)
		if note == "-" {
			note = ""
		}
		if len([]rune(note)) > 120 {
			sendText(bot, chatID, "Комментарий слишком длинный. До 120 символов.")
			return
		}
		if err := addExpense(db, userID, state.CategoryID, state.Amount, note); err != nil {
			sendText(bot, chatID, "Не удалось сохранить расход.")
			return
		}
		delete(states, tgUserID)
		if note == "" {
			sendMainMenu(bot, chatID, fmt.Sprintf("Сохранено: %.2f", state.Amount))
			return
		}
		sendMainMenu(bot, chatID, fmt.Sprintf("Сохранено: %.2f\nКомментарий: %s", state.Amount, note))
	case actionWaitEditAmount:
		amountText := strings.ReplaceAll(strings.TrimSpace(msg.Text), ",", ".")
		amount, err := strconv.ParseFloat(amountText, 64)
		if err != nil || amount <= 0 {
			sendText(bot, chatID, "Введи новую сумму числом, например: 350.50")
			return
		}
		if err := updateExpenseAmount(db, userID, state.ExpenseID, amount); err != nil {
			sendText(bot, chatID, "Не удалось обновить расход.")
			return
		}
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, fmt.Sprintf("Последний расход обновлен: %.2f", amount))
	default:
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, "Состояние сброшено. Выбери действие.")
	}
}

func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, states map[int64]userState, cq *tgbotapi.CallbackQuery) {
	tgUserID := cq.From.ID
	chatID := cq.Message.Chat.ID

	userID, err := ensureUser(db, tgUserID)
	if err != nil {
		answerCallback(bot, cq.ID, "DB error")
		return
	}
	if err := ensureDefaultCategories(db, userID); err != nil {
		answerCallback(bot, cq.ID, "Init error")
		return
	}

	data := cq.Data
	switch {
	case data == "menu_add":
		delete(states, tgUserID)
		if err := sendCategoryKeyboard(bot, db, chatID, userID, "Выбери категорию для расхода:", "pickcat"); err != nil {
			sendText(bot, chatID, "Не удалось загрузить категории.")
		}
	case data == "menu_stats":
		delete(states, tgUserID)
		sendStatsPeriodKeyboard(bot, chatID)
	case data == "menu_cat":
		delete(states, tgUserID)
		states[tgUserID] = userState{Action: actionWaitCustomTitle}
		sendText(bot, chatID, "Введи название новой категории:")
	case data == "menu_export":
		delete(states, tgUserID)
		sendExportPeriodKeyboard(bot, chatID)
	case data == "menu_last":
		delete(states, tgUserID)
		if err := sendLastExpenseKeyboard(bot, db, chatID, userID); err != nil {
			sendText(bot, chatID, "Не удалось загрузить последний расход.")
		}
	case data == "menu_back":
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, "Главное меню")
	case strings.HasPrefix(data, "pickcat:"):
		idStr := strings.TrimPrefix(data, "pickcat:")
		categoryID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			answerCallback(bot, cq.ID, "Неверная категория")
			return
		}
		states[tgUserID] = userState{Action: actionWaitAmount, CategoryID: categoryID}
		sendText(bot, chatID, "Введи сумму расхода (например, 199.90):")
	case strings.HasPrefix(data, "quickcat:"):
		idStr := strings.TrimPrefix(data, "quickcat:")
		categoryID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			answerCallback(bot, cq.ID, "Неверная категория")
			return
		}
		state, ok := states[tgUserID]
		if !ok || state.Action != actionWaitQuickCat {
			sendText(bot, chatID, "Сначала введи расход в формате: 450 кофе")
			return
		}
		if err := addExpense(db, userID, categoryID, state.Amount, state.Note); err != nil {
			sendText(bot, chatID, "Не удалось сохранить быстрый расход.")
			return
		}
		delete(states, tgUserID)
		confirmText := fmt.Sprintf("Сохранено: %.2f\nКатегория выбрана.", state.Amount)
		if state.Note != "" {
			confirmText = fmt.Sprintf("Сохранено: %.2f\nКомментарий: %s", state.Amount, state.Note)
		}
		sendMainMenu(bot, chatID, confirmText)
	case strings.HasPrefix(data, "stats:"):
		period := strings.TrimPrefix(data, "stats:")
		text, err := buildStats(db, userID, period)
		if err != nil {
			sendText(bot, chatID, "Не удалось получить статистику.")
			return
		}
		sendStatsResultWithDetailsKeyboard(bot, chatID, text, period)
	case strings.HasPrefix(data, "statsdetails:"):
		period := strings.TrimPrefix(data, "statsdetails:")
		sendStatsDepthKeyboard(bot, chatID, period)
	case strings.HasPrefix(data, "statsdepth:"):
		payload := strings.TrimPrefix(data, "statsdepth:")
		parts := strings.SplitN(payload, ":", 2)
		if len(parts) != 2 {
			answerCallback(bot, cq.ID, "Неверный формат")
			return
		}
		period := parts[0]
		limitRaw := parts[1]
		limit, err := parseStatsLimit(limitRaw)
		if err != nil {
			answerCallback(bot, cq.ID, "Неверный лимит")
			return
		}
		text, err := buildDetailedStats(db, userID, period, limit)
		if err != nil {
			sendText(bot, chatID, "Не удалось получить развернутую статистику.")
			return
		}
		sendMainMenu(bot, chatID, text)
	case strings.HasPrefix(data, "export:"):
		period := strings.TrimPrefix(data, "export:")
		filename, content, err := buildCSVReport(db, userID, period)
		if err != nil {
			sendText(bot, chatID, "Не удалось сформировать CSV отчет.")
			return
		}
		if err := sendCSVDocument(bot, chatID, filename, content); err != nil {
			sendText(bot, chatID, "Не удалось отправить CSV файл.")
			return
		}
		sendMainMenu(bot, chatID, "CSV отчет отправлен.")
	case strings.HasPrefix(data, "last_delete:"):
		idStr := strings.TrimPrefix(data, "last_delete:")
		expenseID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			answerCallback(bot, cq.ID, "Неверный расход")
			return
		}
		if err := deleteExpenseByID(db, userID, expenseID); err != nil {
			sendText(bot, chatID, "Не удалось удалить расход.")
			return
		}
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, "Последний расход удален.")
	case strings.HasPrefix(data, "last_edit:"):
		idStr := strings.TrimPrefix(data, "last_edit:")
		expenseID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			answerCallback(bot, cq.ID, "Неверный расход")
			return
		}
		states[tgUserID] = userState{Action: actionWaitEditAmount, ExpenseID: expenseID}
		sendText(bot, chatID, "Введи новую сумму для последнего расхода:")
	default:
		answerCallback(bot, cq.ID, "Неизвестное действие")
		return
	}

	answerCallback(bot, cq.ID, "OK")
}

func ensureUser(db *sql.DB, tgUserID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`
INSERT INTO users(tg_user_id) VALUES($1)
ON CONFLICT (tg_user_id) DO UPDATE SET tg_user_id = EXCLUDED.tg_user_id
RETURNING id
`, tgUserID).Scan(&id)
	return id, err
}

func ensureDefaultCategories(db *sql.DB, userID int64) error {
	for _, c := range defaultCategories {
		_, err := db.Exec(`
INSERT INTO categories(user_id, name, is_default)
VALUES($1, $2, TRUE)
ON CONFLICT (user_id, name) DO NOTHING
`, userID, c)
		if err != nil {
			return err
		}
	}
	return nil
}

func addCategory(db *sql.DB, userID int64, name string) error {
	_, err := db.Exec(`
INSERT INTO categories(user_id, name, is_default) VALUES($1, $2, FALSE)
`, userID, name)
	return err
}

func categoriesByUser(db *sql.DB, userID int64) ([]struct {
	ID   int64
	Name string
}, error) {
	rows, err := db.Query(`SELECT id, name FROM categories WHERE user_id = $1 ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []struct {
		ID   int64
		Name string
	}
	for rows.Next() {
		var item struct {
			ID   int64
			Name string
		}
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func addExpense(db *sql.DB, userID, categoryID int64, amount float64, note string) error {
	var exists bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM categories WHERE id = $1 AND user_id = $2)`, categoryID, userID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("category not found")
	}

	_, err = db.Exec(`
INSERT INTO expenses(user_id, category_id, amount, note) VALUES($1, $2, $3, $4)
`, userID, categoryID, amount, note)
	return err
}

func getLastExpense(db *sql.DB, userID int64) (int64, string, float64, string, time.Time, error) {
	var (
		id        int64
		category  string
		amount    float64
		note      string
		createdAt time.Time
	)
	err := db.QueryRow(`
SELECT e.id, c.name, e.amount, e.note, e.created_at
FROM expenses e
JOIN categories c ON c.id = e.category_id
WHERE e.user_id = $1
ORDER BY e.created_at DESC, e.id DESC
LIMIT 1
`, userID).Scan(&id, &category, &amount, &note, &createdAt)
	if err != nil {
		return 0, "", 0, "", time.Time{}, err
	}
	return id, category, amount, note, createdAt, nil
}

func deleteExpenseByID(db *sql.DB, userID, expenseID int64) error {
	res, err := db.Exec(`DELETE FROM expenses WHERE id = $1 AND user_id = $2`, expenseID, userID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("expense not found")
	}
	return nil
}

func updateExpenseAmount(db *sql.DB, userID, expenseID int64, amount float64) error {
	res, err := db.Exec(`UPDATE expenses SET amount = $1 WHERE id = $2 AND user_id = $3`, amount, expenseID, userID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("expense not found")
	}
	return nil
}

func buildStats(db *sql.DB, userID int64, period string) (string, error) {
	now := time.Now()
	var from time.Time
	var label string
	switch period {
	case "today":
		y, m, d := now.Date()
		from = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		label = "за сегодня"
	case "week":
		from = now.AddDate(0, 0, -7)
		label = "за 7 дней"
	case "month":
		from = now.AddDate(0, -1, 0)
		label = "за 30 дней"
	default:
		return "", errors.New("unknown period")
	}

	rows, err := db.Query(`
SELECT c.name, COALESCE(SUM(e.amount), 0) total
FROM expenses e
JOIN categories c ON c.id = e.category_id
WHERE e.user_id = $1 AND e.created_at >= $2
GROUP BY c.name
ORDER BY total DESC
`, userID, from)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	total := 0.0
	lines := []string{fmt.Sprintf("Статистика %s:", label)}
	hasRows := false

	for rows.Next() {
		hasRows = true
		var name string
		var sum float64
		if err := rows.Scan(&name, &sum); err != nil {
			return "", err
		}
		total += sum
		lines = append(lines, fmt.Sprintf("- %s: %.2f", name, sum))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !hasRows {
		return fmt.Sprintf("Пока нет расходов %s.", label), nil
	}
	lines = append(lines, fmt.Sprintf("\nИтого: %.2f", total))
	return strings.Join(lines, "\n"), nil
}

func buildDetailedStats(db *sql.DB, userID int64, period string, limit int) (string, error) {
	from, label, _, err := periodFrom(period)
	if err != nil {
		return "", err
	}

	query := `
SELECT c.name, e.amount, e.note, e.created_at
FROM expenses e
JOIN categories c ON c.id = e.category_id
WHERE e.user_id = $1 AND e.created_at >= $2
ORDER BY e.created_at DESC, e.id DESC
`
	var rows *sql.Rows
	if limit > 0 {
		query += "LIMIT $3"
		rows, err = db.Query(query, userID, from, limit)
	} else {
		rows, err = db.Query(query, userID, from)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()

	title := fmt.Sprintf("Развернутая статистика %s (все записи):", label)
	if limit > 0 {
		title = fmt.Sprintf("Развернутая статистика %s (до %d записей):", label, limit)
	}
	lines := []string{title}
	total := 0.0
	count := 0
	for rows.Next() {
		var category string
		var amount float64
		var note string
		var createdAt time.Time
		if err := rows.Scan(&category, &amount, &note, &createdAt); err != nil {
			return "", err
		}
		count++
		total += amount
		if note == "" {
			note = "—"
		}
		lines = append(lines, fmt.Sprintf("%d) %s | %.2f | %s | %s", count, category, amount, note, createdAt.Format("02.01 15:04")))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return fmt.Sprintf("Пока нет расходов %s.", label), nil
	}
	lines = append(lines, fmt.Sprintf("\nИтого: %.2f", total))
	return strings.Join(lines, "\n"), nil
}

func parseStatsLimit(raw string) (int, error) {
	switch raw {
	case "30":
		return 30, nil
	case "100":
		return 100, nil
	case "all":
		return 0, nil
	default:
		return 0, errors.New("unknown limit")
	}
}

func periodFrom(period string) (time.Time, string, string, error) {
	now := time.Now()
	switch period {
	case "today":
		y, m, d := now.Date()
		from := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		return from, "за сегодня", "today", nil
	case "week":
		return now.AddDate(0, 0, -7), "за 7 дней", "week", nil
	case "month":
		return now.AddDate(0, -1, 0), "за 30 дней", "month", nil
	default:
		return time.Time{}, "", "", errors.New("unknown period")
	}
}

func buildCSVReport(db *sql.DB, userID int64, period string) (string, []byte, error) {
	from, label, slug, err := periodFrom(period)
	if err != nil {
		return "", nil, err
	}

	rows, err := db.Query(`
SELECT e.id, c.name, e.amount, e.note, e.created_at
FROM expenses e
JOIN categories c ON c.id = e.category_id
WHERE e.user_id = $1 AND e.created_at >= $2
ORDER BY e.created_at DESC, e.id DESC
`, userID, from)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()

	buf := &bytes.Buffer{}
	writer := csv.NewWriter(buf)
	if err := writer.Write([]string{"expense_id", "category", "amount", "note", "created_at"}); err != nil {
		return "", nil, err
	}

	total := 0.0
	count := 0
	for rows.Next() {
		var id int64
		var category string
		var amount float64
		var note string
		var createdAt time.Time
		if err := rows.Scan(&id, &category, &amount, &note, &createdAt); err != nil {
			return "", nil, err
		}
		count++
		total += amount
		if err := writer.Write([]string{
			strconv.FormatInt(id, 10),
			category,
			fmt.Sprintf("%.2f", amount),
			note,
			createdAt.Format(time.RFC3339),
		}); err != nil {
			return "", nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}

	if err := writer.Write([]string{}); err != nil {
		return "", nil, err
	}
	if err := writer.Write([]string{"period", label}); err != nil {
		return "", nil, err
	}
	if err := writer.Write([]string{"records", strconv.Itoa(count)}); err != nil {
		return "", nil, err
	}
	if err := writer.Write([]string{"total", fmt.Sprintf("%.2f", total)}); err != nil {
		return "", nil, err
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", nil, err
	}

	filename := fmt.Sprintf("expenses_%s_%s.csv", slug, time.Now().Format("20060102_150405"))
	return filename, buf.Bytes(), nil
}

func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64, text string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Добавить расход", "menu_add"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Статистика", "menu_stats"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗂 Добавить категорию", "menu_cat"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🧾 Последний расход", "menu_last"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📤 Экспорт CSV", "menu_export"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendExportPeriodKeyboard(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Сегодня", "export:today"),
			tgbotapi.NewInlineKeyboardButtonData("7 дней", "export:week"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("30 дней", "export:month"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_back"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "Выбери период для CSV отчета:")
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendLastExpenseKeyboard(bot *tgbotapi.BotAPI, db *sql.DB, chatID, userID int64) error {
	id, category, amount, note, createdAt, err := getLastExpense(db, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			sendMainMenu(bot, chatID, "У тебя пока нет расходов.")
			return nil
		}
		return err
	}

	noteText := note
	if noteText == "" {
		noteText = "—"
	}
	text := fmt.Sprintf("Последний расход:\nКатегория: %s\nСумма: %.2f\nКомментарий: %s\nДата: %s", category, amount, noteText, createdAt.Format("02.01.2006 15:04"))
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Изменить сумму", fmt.Sprintf("last_edit:%d", id)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Удалить", fmt.Sprintf("last_delete:%d", id)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_back"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	_, err = bot.Send(msg)
	return err
}

func sendCategoryKeyboard(bot *tgbotapi.BotAPI, db *sql.DB, chatID, userID int64, text, callbackPrefix string) error {
	categories, err := categoriesByUser(db, userID)
	if err != nil {
		return err
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, c := range categories {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(c.Name, fmt.Sprintf("%s:%d", callbackPrefix, c.ID)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_back"),
	))
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	_, err = bot.Send(msg)
	return err
}

func parseQuickExpenseInput(text string) (float64, string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, "", false
	}
	parts := strings.Fields(trimmed)
	if len(parts) < 2 {
		return 0, "", false
	}
	amountText := strings.ReplaceAll(parts[0], ",", ".")
	amount, err := strconv.ParseFloat(amountText, 64)
	if err != nil || amount <= 0 {
		return 0, "", false
	}
	note := strings.TrimSpace(strings.Join(parts[1:], " "))
	if note == "" {
		return 0, "", false
	}
	return amount, note, true
}

func sendStatsPeriodKeyboard(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Сегодня", "stats:today"),
			tgbotapi.NewInlineKeyboardButtonData("7 дней", "stats:week"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("30 дней", "stats:month"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_back"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "Выбери период:")
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendStatsResultWithDetailsKeyboard(bot *tgbotapi.BotAPI, chatID int64, text, period string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔎 Развернутая с комментариями", fmt.Sprintf("statsdetails:%s", period)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_back"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendStatsDepthKeyboard(bot *tgbotapi.BotAPI, chatID int64, period string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("30", fmt.Sprintf("statsdepth:%s:30", period)),
			tgbotapi.NewInlineKeyboardButtonData("100", fmt.Sprintf("statsdepth:%s:100", period)),
			tgbotapi.NewInlineKeyboardButtonData("все", fmt.Sprintf("statsdepth:%s:all", period)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu_stats"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "Выбери глубину развернутой статистики:")
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendText(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	_, _ = bot.Send(msg)
}

func sendCSVDocument(bot *tgbotapi.BotAPI, chatID int64, filename string, content []byte) error {
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{
		Name:  filename,
		Bytes: content,
	})
	_, err := bot.Send(doc)
	return err
}

func answerCallback(bot *tgbotapi.BotAPI, callbackID, text string) {
	cb := tgbotapi.NewCallback(callbackID, text)
	_, _ = bot.Request(cb)
}
