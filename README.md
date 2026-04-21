# Telegram Expense Bot (Go + Postgres)

Простой Telegram-бот для учета личных расходов:
- полностью работает внутри Telegram;
- управление через inline-кнопки;
- 10 базовых категорий создаются автоматически;
- можно добавлять свои категории;
- можно удалить или отредактировать сумму последнего расхода;
- можно выгрузить CSV-отчет за сегодня, 7 дней или 30 дней;
- есть статистика за сегодня, 7 дней и 30 дней.

## 1) Что нужно
- Docker + Docker Compose plugin на сервере
- SSH-доступ к серверу
- Токен Telegram-бота от [@BotFather](https://t.me/BotFather)

## 2) One-command deploy через SSH
На своей локальной машине (в этой папке) выполни:

```bash
chmod +x deploy.sh
./deploy.sh user@your-server-ip /opt/tg-expenses-bot
```

Скрипт:
- копирует проект на сервер;
- создает `.env` из `.env.docker.example`, если его нет;
- запускает `docker compose up -d --build`.

После первого деплоя на сервере отредактируй:
- `/opt/tg-expenses-bot/.env`

```bash
BOT_TOKEN=...
POSTGRES_DB=tg_expenses
POSTGRES_USER=tg_user
POSTGRES_PASSWORD=...
```

И перезапусти:

```bash
ssh user@your-server-ip "cd /opt/tg-expenses-bot && docker compose up -d --build"
```

## 3) Логи и обновления

Логи:
```bash
ssh user@your-server-ip "cd /opt/tg-expenses-bot && docker compose logs -f bot"
```

Повторный деплой после изменений:
```bash
./deploy.sh user@your-server-ip /opt/tg-expenses-bot
```

## 4) Как пользоваться
1. Напиши боту `/start`
2. Нажми `Добавить расход`
3. Выбери категорию
4. Введи сумму
5. Смотри `Статистика`

## Примечание
Проект сделан максимально простым для локального запуска и использования 1-3 людьми, без сложной архитектуры.
