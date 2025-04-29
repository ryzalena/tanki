package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- Константы ---
const (
	TickRate         = 60 // Обновлений логики в секунду
	BroadcastRate    = 30 // Отправок состояния клиентам в секунду
	GameWidth        = 800
	GameHeight       = 600
	PlayerSpeed      = 150 // Пикселей в секунду
	PlayerRadius     = 15
	ProjectileSpeed  = 300 // Пикселей в секунду
	ProjectileRadius = 3
	ShootCooldown    = time.Millisecond * 500 // Задержка между выстрелами
	InitialLives = 15 // изначальное колво жизней
)

// --- Структуры данных ---

// PlayerInput хранит текущее состояние управляющих клавиш игрока
type PlayerInput struct {
	Up    bool    `json:"up"`
	Down  bool    `json:"down"`
	Left  bool    `json:"left"`
	Right bool    `json:"right"`
	AimX  float64 `json:"aimX"` // X координата прицела
	AimY  float64 `json:"aimY"` // Y координата прицела
}

// Player представляет игрока
type Player struct {
	ID           string          `json:"id"`
	X            float64         `json:"x"`
	Y            float64         `json:"y"`
	Color        string          `json:"color"`
	Score        int             `json:"score"`
	Lives        int             `json:"lives"` // добавлено после для жизни
	Nickname     string          `json:"nickname"` // Добавлено поле для никнейма
	BodyAngle    float64         `json:"bodyAngle"` // Угол корпуса танка
	AimAngle     float64         `json:"aimAngle"` // Угол прицеливания игрока
	Input        PlayerInput     `json:"-"`        // Текущий ввод игрока (обновляется клиентом)
	LastShotTime time.Time       `json:"-"`        // Время последнего выстрела (серверная логика)
	WantsToShoot bool            `json:"-"`        // Флаг, что игрок хочет выстрелить
	Conn         *websocket.Conn `json:"-"`        // Ссылка на соединение
	MessageChan  chan []byte     `json:"-"`        // Канал для отправки сообщений этому игроку
}

// ShootCommand передает направление выстрела
type ShootCommand struct {
    DirectionX float64 `json:"directionX"` // Нормализованный вектор X
    DirectionY float64 `json:"directionY"` // Нормализованный вектор Y
}

// Projectile представляет снаряд
type Projectile struct {
	ID      string  `json:"id"`
	OwnerID string  `json:"ownerId"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	VX      float64 `json:"-"` // Скорость по X
	VY      float64 `json:"-"` // Скорость по Y
}

// GameState хранит все состояние игры
type GameState struct {
	Players     map[string]*Player
	Projectiles map[string]*Projectile
	Bounds      struct{ Width, Height int }
	mutex       sync.RWMutex // RWMutex для частых чтений (трансляция) и редких записей
}

// --- Сообщения WebSocket ---

// ClientMessage - сообщение от клиента
type ClientMessage struct {
	Action  string          `json:"action"`  // "input", "shoot"
	Payload json.RawMessage `json:"payload"` // PlayerInput для "input", ShootCommand для "shoot"
}

// ServerMessage - сообщение от сервера
type ServerMessage struct {
	Type    string      `json:"type"`    // "gameState", "assignId", "error"
	Payload interface{} `json:"payload"` // Зависит от типа
}

// GameStatePayload - структура для отправки состояния клиентам
type GameStatePayload struct {
	Players     []*Player     `json:"players"`
	Projectiles []*Projectile `json:"projectiles"`
}

// --- Глобальные переменные ---
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Разрешаем все источники
}

var game = &GameState{ // Единственный экземпляр игры
	Players:     make(map[string]*Player),
	Projectiles: make(map[string]*Projectile),
	Bounds:      struct{ Width, Height int }{GameWidth, GameHeight},
}

var nextPlayerID = 1     // Простой счетчик ID игроков
var nextProjectileID = 1 // Простой счетчик ID снарядов

// --- Вспомогательные функции ---
func generateID(prefix string, counter *int) string {
	id := fmt.Sprintf("%s%d", prefix, *counter)
	*counter++
	return id
}

func randomColor() string {
	return fmt.Sprintf("#%06x", rand.Intn(0xFFFFFF))
}

// calculateDirection вычисляет нормализованный направляющий вектор
func calculateDirection(fromX, fromY, toX, toY float64) (float64, float64) {
	dx := toX - fromX
	dy := toY - fromY
	length := math.Sqrt(dx*dx + dy*dy)

	// Если длина слишком маленькая, стреляем вправо по умолчанию
	if length < 0.001 {
		return 1.0, 0.0
	}

	return dx / length, dy / length
}

// --- Логика Игры ---

// gameLoop - основной цикл обновления логики игры
func gameLoop() {
	ticker := time.NewTicker(time.Second / TickRate)
	defer ticker.Stop()

	var lastTick time.Time = time.Now()

	for range ticker.C {
		now := time.Now()
		deltaTime := now.Sub(lastTick).Seconds() // Время с прошлого тика
		lastTick = now

		updateGameLogic(deltaTime)
	}
}

// updateGameLogic - обновляет состояние всех объектов игры
func updateGameLogic(dt float64) {
	game.mutex.Lock() // Полная блокировка на время обновления
	defer game.mutex.Unlock()

	projectilesToRemove := []string{}

	// Обновляем игроков
	for _, player := range game.Players {
		// Движение
		targetVX, targetVY := 0.0, 0.0
		if player.Input.Up {
			targetVY -= PlayerSpeed
		}
		if player.Input.Down {
			targetVY += PlayerSpeed
		}
		if player.Input.Left {
			targetVX -= PlayerSpeed
		}
		if player.Input.Right {
			targetVX += PlayerSpeed
		}

		// Нормализация диагональной скорости (простая)
		if targetVX != 0 && targetVY != 0 {
			factor := 1.0 / math.Sqrt(2.0)
			targetVX *= factor
			targetVY *= factor
		}

		player.X += targetVX * dt
		player.Y += targetVY * dt

		// Ограничение по границам
		player.X = math.Max(PlayerRadius, math.Min(float64(game.Bounds.Width-PlayerRadius), player.X))
		player.Y = math.Max(PlayerRadius, math.Min(float64(game.Bounds.Height-PlayerRadius), player.Y))

		// Обновление угла прицеливания на основе данных ввода
		if player.Input.AimX != 0 || player.Input.AimY != 0 {
			player.AimAngle = math.Atan2(player.Input.AimY-player.Y, player.Input.AimX-player.X)
			
			// Обновляем угол корпуса только при движении
			if player.Input.Up || player.Input.Down || player.Input.Left || player.Input.Right {
				player.BodyAngle = math.Atan2(targetVY, targetVX)
			}
		}

		// Стрельба
		if player.WantsToShoot && time.Since(player.LastShotTime) >= ShootCooldown {
			player.LastShotTime = time.Now()
			player.WantsToShoot = false // Сбрасываем флаг

			// Определяем направление выстрела на основе угла прицеливания
			dirX := math.Cos(player.AimAngle)
			dirY := math.Sin(player.AimAngle)

			projID := generateID("p", &nextProjectileID)
			newProj := &Projectile{
				ID:      projID,
				OwnerID: player.ID,
				X:       player.X, // Начальная позиция - центр игрока
				Y:       player.Y,
				VX:      dirX * ProjectileSpeed,
				VY:      dirY * ProjectileSpeed,
			}
			game.Projectiles[projID] = newProj
			log.Printf("Игрок %s выстрелил снаряд %s под углом %.2f", player.ID, projID, player.AimAngle)
		}
	}

	// Обновляем снаряды и проверяем коллизии
	for id, proj := range game.Projectiles {
		proj.X += proj.VX * dt
		proj.Y += proj.VY * dt

		// Удаление за границами
		if proj.X < 0 || proj.X > float64(game.Bounds.Width) || proj.Y < 0 || proj.Y > float64(game.Bounds.Height) {
			projectilesToRemove = append(projectilesToRemove, id)
			continue
		}

		// Проверка столкновения с игроками
		for playerID, player := range game.Players {
			if proj.OwnerID == playerID {
				continue
			} // Не сталкиваемся с собой

			distSq := math.Pow(proj.X-player.X, 2) + math.Pow(proj.Y-player.Y, 2)
			radiiSq := math.Pow(PlayerRadius+ProjectileRadius, 2)

			if distSq < radiiSq {
				log.Printf("Снаряд %s попал в игрока %s!", id, playerID)
				projectilesToRemove = append(projectilesToRemove, id) // Удаляем снаряд

				// Уменьшаем жизни игрока
				player.Lives--
				log.Printf("Игрок %s теряет жизнь. Осталось: %d", playerID, player.Lives)

				// Начисляем очки стрелявшему
				if shooter, ok := game.Players[proj.OwnerID]; ok {
					shooter.Score++
					log.Printf("Игрок %s получает очко! Счет: %d", shooter.ID, shooter.Score)
				}
				// TODO: Можно добавить эффект для игрока, в которого попали (например, респаун)
				break // Снаряд может попасть только в одного игрока за тик
			}
		}
	}

	// Удаляем помеченные снаряды
	for _, id := range projectilesToRemove {
		delete(game.Projectiles, id)
	}
}

// broadcastLoop - рассылает состояние игры клиентам
func broadcastLoop() {
	ticker := time.NewTicker(time.Second / BroadcastRate)
	defer ticker.Stop()

	for range ticker.C {
		sendGameStateToAll()
	}
}

// sendGameStateToAll - готовит и отправляет состояние всем
func sendGameStateToAll() {
	game.mutex.RLock() // Блокировка чтения - другие читатели не блокируются
	defer game.mutex.RUnlock()

	// Создаем срезы для JSON (карты не гарантируют порядок в JSON)
	playerList := make([]*Player, 0, len(game.Players))
	for _, p := range game.Players {
		playerList = append(playerList, p)
	}
	projectileList := make([]*Projectile, 0, len(game.Projectiles))
	for _, p := range game.Projectiles {
		projectileList = append(projectileList, p)
	}

	payload := GameStatePayload{
		Players:     playerList,
		Projectiles: projectileList,
	}
	msg := ServerMessage{Type: "gameState", Payload: payload}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Ошибка маршалинга gameState: %v", err)
		return
	}

	// Отправляем сообщение в канал каждого игрока
	for _, player := range game.Players {
		// Используем неблокирующую отправку, чтобы не зависнуть, если канал переполнен
		select {
		case player.MessageChan <- msgBytes:
		default:
			log.Printf("Предупреждение: Канал сообщений для игрока %s переполнен или закрыт.", player.ID)
		}
	}
}

// --- Обработка WebSocket ---

// handleConnections - обрабатывает новые подключения
func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Ошибка обновления до WebSocket: %v", err)
		return
	}

	log.Printf("Новое WebSocket соединение: %s", conn.RemoteAddr())

	// Создаем нового игрока
	game.mutex.Lock() // Блокируем для записи
	playerID := generateID("plr", &nextPlayerID)
	player := &Player{
		ID:           playerID,
		X:            float64(rand.Intn(GameWidth-PlayerRadius*2) + PlayerRadius), // Случайная позиция
		Y:            float64(rand.Intn(GameHeight-PlayerRadius*2) + PlayerRadius),
		Color:        randomColor(),
		Score:        0,
		Lives:        InitialLives, // устанавливаем начальное колво жизней
		AimAngle:     0, // По умолчанию смотрим вправо
		Conn:         conn,
		MessageChan:  make(chan []byte, 32),          // Буферизованный канал
		LastShotTime: time.Now().Add(-ShootCooldown), // Чтобы можно было стрелять сразу
		Nickname:     "Player " + playerID, // Дефолтное имя
	}
	game.Players[playerID] = player
	log.Printf("Создан игрок %s для %s", playerID, conn.RemoteAddr())
	game.mutex.Unlock()

	// Отправляем ID новому клиенту
	assignMsg := ServerMessage{Type: "assignId", Payload: map[string]string{"id": playerID}}
	assignBytes, _ := json.Marshal(assignMsg)
	select {
	case player.MessageChan <- assignBytes:
	default: // Если не удалось отправить сразу - вероятно, канал уже закрыт
	}

	// Запускаем горутины для чтения и записи для этого клиента
	go writer(player)
	go reader(player)
}

// reader - читает сообщения от клиента
func reader(player *Player) {
	conn := player.Conn
	playerID := player.ID

	defer func() {
		log.Printf("Reader завершается для игрока %s (%s)", playerID, conn.RemoteAddr())
		game.mutex.Lock()
		delete(game.Players, playerID) // Удаляем игрока из игры
		close(player.MessageChan)      // Закрываем канал записи
		conn.Close()                   // Закрываем соединение
		log.Printf("Игрок %s удален.", playerID)
		game.mutex.Unlock()
	}()

	conn.SetReadLimit(512)

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Неожиданная ошибка чтения для %s: %v", playerID, err)
			} else {
				log.Printf("Соединение %s закрыто: %v", playerID, err)
			}
			break
		}

		if messageType != websocket.TextMessage {
			log.Printf("Получено не текстовое сообщение от %s", playerID)
			continue
		}

		var msg ClientMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Ошибка парсинга JSON от %s: %v", playerID, err)
			continue
		}

		// Обновляем состояние игрока (ввод/стрельба)
		game.mutex.Lock()
		if p, ok := game.Players[playerID]; ok {
			switch msg.Action {
			case "setNickname":
				var nicknamePayload struct {
					Nickname string `json:"nickname"`
				}
				if err := json.Unmarshal(msg.Payload, &nicknamePayload); err == nil {
					p.Nickname = nicknamePayload.Nickname
					log.Printf("Игрок %s установил никнейм: %s", playerID, p.Nickname)
				}
			case "input":
				// Нужно аккуратно распаковать payload в PlayerInput
				var inputPayload PlayerInput
				if err := json.Unmarshal(msg.Payload, &inputPayload); err == nil {
					p.Input = inputPayload
					// Обновляем угол прицеливания
					if inputPayload.AimX != 0 || inputPayload.AimY != 0 {
						p.AimAngle = math.Atan2(inputPayload.AimY-p.Y, inputPayload.AimX-p.X)
					}
				} else {
					log.Printf("Ошибка парсинга input payload от %s: %v", playerID, err)
				}
			case "shoot":
				// Парсим команду выстрела с координатами прицела
				var shootCmd ShootCommand
				if err := json.Unmarshal(msg.Payload, &shootCmd); err == nil {
					// Обновляем только угол пушки (aimAngle)
					p.AimAngle = math.Atan2(shootCmd.DirectionY, shootCmd.DirectionX)
					p.WantsToShoot = true
				} else {
					log.Printf("Ошибка парсинга shoot payload от %s: %v", playerID, err)
					p.WantsToShoot = true // Стреляем в текущем направлении, если парсинг не удался
				}
			default:
				log.Printf("Неизвестное действие '%s' от %s", msg.Action, playerID)
			}
		}
		game.mutex.Unlock()
	}
}

// writer - пишет сообщения из канала игрока в WebSocket соединение
func writer(player *Player) {
	conn := player.Conn
	playerID := player.ID
	messageChan := player.MessageChan

	defer func() {
		log.Printf("Writer завершается для игрока %s (%s)", playerID, conn.RemoteAddr())
	}()

	for message := range messageChan { // Цикл работает, пока канал не будет закрыт (в reader)
		err := conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Printf("Ошибка записи сообщения игроку %s: %v", playerID, err)
			return
		}
	}
}

// --- Точка входа ---
func main() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Println("======================================")
	log.Println(" Запуск сервера Динамической Игры ")
	log.Println("======================================")

	// Запускаем игровые циклы
	go gameLoop()
	go broadcastLoop()

	// Настройка HTTP сервера с обработкой статических файлов
	fs := http.FileServer(http.Dir("./static"))               // Обслуживаем файлы из текущей директории
	http.Handle("/static/", http.StripPrefix("/static/", fs)) // Префикс для статических файлов

	http.HandleFunc("/ws", handleConnections)
	// новую ручку ктр будет выводить логин пользователя
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Проверяем существование файла
		if r.URL.Path == "/" {
			
			http.ServeFile(w, r, "index.html")
			return
		}

		// Для всех остальных запросов пробуем найти файл
		path := filepath.Join(".", r.URL.Path)
		fmt.Println(path)
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		http.ServeFile(w, r, path)
	})

	log.Println("Сервер слушает на http://localhost:8080")
	log.Println("Доступные файлы:")
	files, _ := filepath.Glob("*")
	for _, file := range files {
		log.Printf(" - %s", file)
	}

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Критическая ошибка ListenAndServe: ", err)
	}
}
