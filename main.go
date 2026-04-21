package main

import (
	"database/sql"
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
}

const (
	actionWaitAmount      = "wait_amount"
	actionWaitCustomTitle = "wait_custom_title"
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
		if err := addExpense(db, userID, state.CategoryID, amount); err != nil {
			sendText(bot, chatID, "Не удалось сохранить расход.")
			return
		}
		delete(states, tgUserID)
		sendMainMenu(bot, chatID, fmt.Sprintf("Сохранено: %.2f", amount))
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
		if err := sendCategoryKeyboard(bot, db, chatID, userID, "Выбери категорию для расхода:"); err != nil {
			sendText(bot, chatID, "Не удалось загрузить категории.")
		}
	case data == "menu_stats":
		delete(states, tgUserID)
		sendStatsPeriodKeyboard(bot, chatID)
	case data == "menu_cat":
		delete(states, tgUserID)
		states[tgUserID] = userState{Action: actionWaitCustomTitle}
		sendText(bot, chatID, "Введи название новой категории:")
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
	case strings.HasPrefix(data, "stats:"):
		period := strings.TrimPrefix(data, "stats:")
		text, err := buildStats(db, userID, period)
		if err != nil {
			sendText(bot, chatID, "Не удалось получить статистику.")
			return
		}
		sendMainMenu(bot, chatID, text)
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

func addExpense(db *sql.DB, userID, categoryID int64, amount float64) error {
	var exists bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM categories WHERE id = $1 AND user_id = $2)`, categoryID, userID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("category not found")
	}

	_, err = db.Exec(`
INSERT INTO expenses(user_id, category_id, amount) VALUES($1, $2, $3)
`, userID, categoryID, amount)
	return err
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
	)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	_, _ = bot.Send(msg)
}

func sendCategoryKeyboard(bot *tgbotapi.BotAPI, db *sql.DB, chatID, userID int64, text string) error {
	categories, err := categoriesByUser(db, userID)
	if err != nil {
		return err
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, c := range categories {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(c.Name, fmt.Sprintf("pickcat:%d", c.ID)),
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

func sendText(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	_, _ = bot.Send(msg)
}

func answerCallback(bot *tgbotapi.BotAPI, callbackID, text string) {
	cb := tgbotapi.NewCallback(callbackID, text)
	_, _ = bot.Request(cb)
}
