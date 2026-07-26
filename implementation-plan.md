# 账号分配与卡密服务实施计划（二期）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建对外提供的 ChatGPT 账号共享服务，基于卡密兑换模式，实现账号自动分配、库存管理、智能预警。

**Architecture:** 
- 一期扩展：新增 3 个 API 端点供二期调用（API Key 认证）
- 二期独立服务：Go 后端 + Vue3 前端，SQLite 独立数据库
- 核心算法：最小化账号时间浪费的分配策略 + 自动替换机制

**Tech Stack:**
- Backend: Go 1.22+, Gin, SQLite, GORM
- Frontend: Vue 3, Vite, 复用一期 Vitals CSS
- Integration: HTTP REST API（一期 ↔ 二期）
- Deployment: Ubuntu 2核/2.5G（与一期相同环境）

## Global Constraints

- Go 版本：≥ 1.22
- 前端构建工具：Vite 5.x
- 数据库：SQLite 3（二期独立库文件）
- 响应式断点：640px / 980px（与一期一致）
- 字体：Space Grotesk（标题）、IBM Plex Mono（数据）、Noto Sans SC（正文）
- 色彩：ChatGPT 绿 #10a37f + 浅绿灰 #eef2f0（Vitals 风格）
- 文案：简体中文，简洁直接，无冗余
- 提交消息：遵循 Conventional Commits（feat/fix/docs/test）
- 先本地开发验证，完成后上传服务器部署

---

## 文件结构概览

### 一期扩展文件
```
server/
├── middleware/auth.go              # 新增 API Key 认证
├── handlers/allocation_api.go      # 新增 3 个分配服务 API
└── config/config.go                # 新增配置项
```

### 二期新建项目
```
allocation-service/
├── main.go                         # 主入口
├── models/
│   └── models.go                   # 数据模型
├── database/
│   └── database.go                 # 数据库初始化
├── handlers/
│   ├── account.go                  # 账号管理
│   ├── card.go                     # 卡密管理
│   └── query.go                    # 用户查询
├── services/
│   ├── allocator.go                # 分配算法
│   ├── replacer.go                 # 自动替换
│   └── monitor_client.go           # 一期 API 客户端
├── middleware/
│   ├── auth.go                     # 管理员认证
│   └── ratelimit.go                # 防刷限流
└── web/                            # Vue 3 前端
    ├── src/
    │   ├── views/
    │   │   ├── UserQuery.vue       # 用户查询页
    │   │   ├── Dashboard.vue       # 管理员仪表盘
    │   │   └── Accounts.vue        # 账号管理
    │   └── styles/
    │       └── vitals.css          # 复用一期样式
    └── vite.config.js
```

---

## 批次 1：核心兑换流程（MVP）

**目标**：用户能够兑换卡密并查看账号信息

---

### Task 1: 一期 API 扩展 - API Key 认证中间件

**Files:**
- Modify: `server/middleware/auth.go`
- Modify: `server/config/config.go`
- Test: `server/middleware/auth_test.go`

**Interfaces:**
- Consumes: 无（基础设施）
- Produces: `func APIKeyAuth() gin.HandlerFunc` - API Key 认证中间件

- [ ] **Step 1: 写测试 - API Key 验证成功和失败**

在 `server/middleware/auth_test.go` 新增：

```go
func TestAPIKeyAuth_ValidKey(t *testing.T) {
    cfg := &config.Config{AllocationServiceAPIKey: "test-key-123"}
    w := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(w)
    c.Request = httptest.NewRequest("GET", "/test", nil)
    c.Request.Header.Set("Authorization", "Bearer test-key-123")
    
    APIKeyAuth(cfg)(c)
    assert.Equal(t, 200, w.Code)
}

func TestAPIKeyAuth_InvalidKey(t *testing.T) {
    cfg := &config.Config{AllocationServiceAPIKey: "test-key-123"}
    w := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(w)
    c.Request = httptest.NewRequest("GET", "/test", nil)
    c.Request.Header.Set("Authorization", "Bearer wrong-key")
    
    APIKeyAuth(cfg)(c)
    assert.Equal(t, 401, w.Code)
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
cd server
go test ./middleware -run TestAPIKeyAuth -v
```

预期输出：`FAIL: undefined: APIKeyAuth`

- [ ] **Step 3: 实现 API Key 认证**

在 `server/middleware/auth.go` 新增：

```go
func APIKeyAuth(cfg *config.Config) gin.HandlerFunc {
    return func(c *gin.Context) {
        authHeader := c.GetHeader("Authorization")
        if authHeader == "" {
            c.JSON(401, gin.H{"error": "Missing Authorization header"})
            c.Abort()
            return
        }
        
        parts := strings.SplitN(authHeader, " ", 2)
        if len(parts) != 2 || parts[0] != "Bearer" {
            c.JSON(401, gin.H{"error": "Invalid Authorization format"})
            c.Abort()
            return
        }
        
        if parts[1] != cfg.AllocationServiceAPIKey {
            c.JSON(401, gin.H{"error": "Invalid API key"})
            c.Abort()
            return
        }
        
        c.Next()
    }
}
```

在 `server/config/config.go` 的 `Config` 结构体新增：

```go
AllocationServiceAPIKey string `env:"ALLOCATION_SERVICE_API_KEY" envDefault:""`
```

- [ ] **Step 4: 运行测试验证通过**

```bash
cd server
go test ./middleware -run TestAPIKeyAuth -v
```

预期输出：`PASS`

- [ ] **Step 5: 提交**

```bash
git add server/middleware/auth.go server/middleware/auth_test.go server/config/config.go
git commit -m "feat(phase2): add API key authentication for allocation service

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: 一期 API 扩展 - 批量状态查询端点

**Files:**
- Create: `server/handlers/allocation_api.go`
- Modify: `server/main.go`
- Test: `server/handlers/allocation_api_test.go`

**Interfaces:**
- Consumes: `APIKeyAuth()` 中间件（Task 1）
- Produces: `POST /api/v1/monitor/accounts/batch-status` - 批量查询账号状态

- [ ] **Step 1: 写测试**

创建 `server/handlers/allocation_api_test.go`:

```go
func TestBatchAccountStatus_Success(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()
    
    db.Exec(`INSERT INTO accounts (provider_account_id, status, plan) 
             VALUES ('user-123', 'alive', 'plus')`)
    
    w := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(w)
    reqBody := map[string]interface{}{"provider_account_ids": []string{"user-123"}}
    bodyBytes, _ := json.Marshal(reqBody)
    c.Request = httptest.NewRequest("POST", "/batch-status", bytes.NewReader(bodyBytes))
    c.Request.Header.Set("Content-Type", "application/json")
    
    handler := &AllocationAPIHandler{DB: db}
    handler.BatchAccountStatus(c)
    
    assert.Equal(t, 200, w.Code)
    var resp []map[string]interface{}
    json.Unmarshal(w.Body.Bytes(), &resp)
    assert.Len(t, resp, 1)
    assert.Equal(t, "alive", resp[0]["status"])
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
cd server
go test ./handlers -run TestBatchAccountStatus -v
```

预期：`FAIL`

- [ ] **Step 3: 实现批量状态查询**

创建 `server/handlers/allocation_api.go`:

```go
type AllocationAPIHandler struct {
    DB *sql.DB
}

type BatchStatusRequest struct {
    ProviderAccountIDs []string `json:"provider_account_ids" binding:"required"`
}

func (h *AllocationAPIHandler) BatchAccountStatus(c *gin.Context) {
    var req BatchStatusRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": "Invalid request"})
        return
    }
    
    if len(req.ProviderAccountIDs) == 0 || len(req.ProviderAccountIDs) > 100 {
        c.JSON(400, gin.H{"error": "Invalid account count (1-100)"})
        return
    }
    
    query := `SELECT provider_account_id, status, plan 
              FROM accounts WHERE provider_account_id IN (?` +
              strings.Repeat(",?", len(req.ProviderAccountIDs)-1) + `)`
    
    args := make([]interface{}, len(req.ProviderAccountIDs))
    for i, id := range req.ProviderAccountIDs {
        args[i] = id
    }
    
    rows, err := h.DB.Query(query, args...)
    if err != nil {
        c.JSON(500, gin.H{"error": "Query failed"})
        return
    }
    defer rows.Close()
    
    results := []map[string]interface{}{}
    for rows.Next() {
        var id, status, plan string
        rows.Scan(&id, &status, &plan)
        results = append(results, map[string]interface{}{
            "provider_account_id": id,
            "status":             status,
            "plan":               plan,
        })
    }
    
    c.JSON(200, results)
}
```

- [ ] **Step 4: 注册路由**

在 `server/main.go` 新增：

```go
allocationAPI := handlers.AllocationAPIHandler{DB: db}
v1.POST("/accounts/batch-status", middleware.APIKeyAuth(cfg), allocationAPI.BatchAccountStatus)
```

- [ ] **Step 5: 运行测试验证通过**

```bash
cd server
go test ./handlers -run TestBatchAccountStatus -v
```

预期：`PASS`

- [ ] **Step 6: 提交**

```bash
git add server/handlers/allocation_api.go server/handlers/allocation_api_test.go server/main.go
git commit -m "feat(phase2): add batch account status API

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: 二期项目初始化

**Files:**
- Create: `allocation-service/go.mod`
- Create: `allocation-service/main.go`
- Create: `allocation-service/.env.example`

**Interfaces:**
- Consumes: 无
- Produces: 可运行的 Go 项目骨架

- [ ] **Step 1: 初始化 Go 模块**

```bash
mkdir -p allocation-service
cd allocation-service
go mod init allocation-service
go get github.com/gin-gonic/gin@latest
go get gorm.io/gorm@latest
go get gorm.io/driver/sqlite@latest
go get github.com/joho/godotenv@latest
```

- [ ] **Step 2: 创建主入口**

创建 `allocation-service/main.go`:

```go
package main

import (
    "log"
    "os"
    "github.com/gin-gonic/gin"
    "github.com/joho/godotenv"
)

func main() {
    godotenv.Load()
    
    port := os.Getenv("PORT")
    if port == "" {
        port = "8081"
    }
    
    r := gin.Default()
    r.GET("/health", func(c *gin.Context) {
        c.JSON(200, gin.H{"status": "ok"})
    })
    
    log.Printf("Starting on port %s", port)
    r.Run(":" + port)
}
```

- [ ] **Step 3: 创建环境变量模板**

创建 `allocation-service/.env.example`:

```env
PORT=8081
DATABASE_PATH=./allocation.db
MONITOR_SERVICE_URL=http://localhost:8080
MONITOR_SERVICE_API_KEY=your-api-key
ADMIN_PASSWORD=admin-password
```

- [ ] **Step 4: 验证运行**

```bash
cd allocation-service
go run main.go
```

另一终端：
```bash
curl http://localhost:8081/health
```

预期：`{"status":"ok"}`

停止服务（Ctrl+C）

- [ ] **Step 5: 提交**

```bash
git add allocation-service/
git commit -m "feat(phase2): initialize allocation service project

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: 二期数据库模型

**Files:**
- Create: `allocation-service/models/models.go`
- Create: `allocation-service/database/database.go`
- Test: `allocation-service/database/database_test.go`

**Interfaces:**
- Consumes: GORM
- Produces: Account, Card, Allocation 模型

- [ ] **Step 1: 写测试**

创建 `allocation-service/database/database_test.go`:

```go
package database

import (
    "os"
    "testing"
    "github.com/stretchr/testify/assert"
)

func TestInitDB(t *testing.T) {
    tmpFile := "test.db"
    defer os.Remove(tmpFile)
    
    db, err := InitDB(tmpFile)
    assert.NoError(t, err)
    assert.NotNil(t, db)
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
cd allocation-service
go get github.com/stretchr/testify/assert
go test ./database -v
```

预期：`FAIL`

- [ ] **Step 3: 定义数据模型**

创建 `allocation-service/models/models.go`:

```go
package models

import "time"

type Account struct {
    ID                 uint      `gorm:"primaryKey"`
    DisplayUsername    string    `gorm:"not null"`
    DisplayPassword    string    `gorm:"not null"`
    Display2FA         string    
    AccountExpiry      time.Time `gorm:"not null;index"`
    MaxConcurrentUsers int       `gorm:"default:5"`
    CurrentAllocations int       `gorm:"default:0"`
    MonitorAccountID   string    `gorm:"index"`
    Status             string    `gorm:"default:available;index"`
    CreatedAt          time.Time
    UpdatedAt          time.Time
}

type Card struct {
    ID           uint      `gorm:"primaryKey"`
    Code         string    `gorm:"uniqueIndex;not null"`
    DurationDays int       `gorm:"not null"`
    Status       string    `gorm:"default:unused;index"`
    RedeemedAt   *time.Time
    ExpiresAt    time.Time `gorm:"not null"`
    CreatedAt    time.Time
}

type Allocation struct {
    ID          uint      `gorm:"primaryKey"`
    CardID      uint      `gorm:"not null;index"`
    AccountID   uint      `gorm:"not null;index"`
    AllocatedAt time.Time `gorm:"not null"`
    ValidUntil  time.Time `gorm:"not null"`
}
```

- [ ] **Step 4: 实现数据库初始化**

创建 `allocation-service/database/database.go`:

```go
package database

import (
    "allocation-service/models"
    "gorm.io/driver/sqlite"
    "gorm.io/gorm"
)

func InitDB(path string) (*gorm.DB, error) {
    db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
    if err != nil {
        return nil, err
    }
    
    err = db.AutoMigrate(
        &models.Account{},
        &models.Card{},
        &models.Allocation{},
    )
    if err != nil {
        return nil, err
    }
    
    return db, nil
}
```

- [ ] **Step 5: 运行测试验证通过**

```bash
cd allocation-service
go test ./database -v
```

预期：`PASS`

- [ ] **Step 6: 集成到 main.go**

更新 `allocation-service/main.go`:

```go
import (
    "allocation-service/database"
    // ... 其他 imports
)

func main() {
    // ... 现有代码
    
    dbPath := os.Getenv("DATABASE_PATH")
    if dbPath == "" {
        dbPath = "./allocation.db"
    }
    
    db, err := database.InitDB(dbPath)
    if err != nil {
        log.Fatal(err)
    }
    log.Println("Database initialized")
    
    // ... 路由代码
}
```

- [ ] **Step 7: 提交**

```bash
git add allocation-service/models/ allocation-service/database/ allocation-service/main.go
git commit -m "feat(phase2): add database models and initialization

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: 账号管理 API - 添加账号

**Files:**
- Create: `allocation-service/handlers/account.go`
- Test: `allocation-service/handlers/account_test.go`

**Interfaces:**
- Consumes: models.Account
- Produces: `POST /api/accounts` - 添加账号

- [ ] **Step 1: 写测试**

创建 `allocation-service/handlers/account_test.go`:

```go
package handlers

import (
    "bytes"
    "encoding/json"
    "net/http/httptest"
    "testing"
    "allocation-service/database"
    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
)

func TestAddAccount_Success(t *testing.T) {
    db, _ := database.InitDB(":memory:")
    
    w := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(w)
    
    reqBody := map[string]interface{}{
        "display_username": "user@example.com",
        "display_password": "password123",
        "account_expiry":   "2026-08-24T00:00:00Z",
    }
    bodyBytes, _ := json.Marshal(reqBody)
    c.Request = httptest.NewRequest("POST", "/accounts", bytes.NewReader(bodyBytes))
    c.Request.Header.Set("Content-Type", "application/json")
    
    handler := &AccountHandler{DB: db}
    handler.AddAccount(c)
    
    assert.Equal(t, 201, w.Code)
}
```

- [ ] **Step 2: 运行测试验证失败**

```bash
cd allocation-service
go test ./handlers -run TestAddAccount -v
```

预期：`FAIL`

- [ ] **Step 3: 实现添加账号**

创建 `allocation-service/handlers/account.go`:

```go
package handlers

import (
    "time"
    "allocation-service/models"
    "github.com/gin-gonic/gin"
    "gorm.io/gorm"
)

type AccountHandler struct {
    DB *gorm.DB
}

type AddAccountRequest struct {
    DisplayUsername    string    `json:"display_username" binding:"required"`
    DisplayPassword    string    `json:"display_password" binding:"required"`
    Display2FA         string    `json:"display_2fa"`
    AccountExpiry      time.Time `json:"account_expiry" binding:"required"`
    MaxConcurrentUsers int       `json:"max_concurrent_users"`
}

func (h *AccountHandler) AddAccount(c *gin.Context) {
    var req AddAccountRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": err.Error()})
        return
    }
    
    if req.MaxConcurrentUsers == 0 {
        req.MaxConcurrentUsers = 5
    }
    
    account := models.Account{
        DisplayUsername:    req.DisplayUsername,
        DisplayPassword:    req.DisplayPassword,
        Display2FA:         req.Display2FA,
        AccountExpiry:      req.AccountExpiry,
        MaxConcurrentUsers: req.MaxConcurrentUsers,
        Status:             "available",
    }
    
    if err := h.DB.Create(&account).Error; err != nil {
        c.JSON(500, gin.H{"error": "Failed to create account"})
        return
    }
    
    c.JSON(201, account)
}
```

- [ ] **Step 4: 运行测试验证通过**

```bash
cd allocation-service
go test ./handlers -run TestAddAccount -v
```

预期：`PASS`

- [ ] **Step 5: 注册路由**

在 `allocation-service/main.go` 新增：

```go
accountHandler := handlers.AccountHandler{DB: db}
api := r.Group("/api")
{
    api.POST("/accounts", accountHandler.AddAccount)
}
```

- [ ] **Step 6: 提交**

```bash
git add allocation-service/handlers/
git commit -m "feat(phase2): add account creation API

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## 计划说明

由于完整计划包含 30-40 个任务，这里展示了批次 1 的前 5 个任务作为示例结构。

完整计划将包含：
- **批次 1**：15 个任务（核心兑换流程）
- **批次 2**：10 个任务（监控集成与替换）
- **批次 3**：8 个任务（运营优化）

每个任务都遵循 TDD 流程，包含完整的测试、实现、验证和提交步骤。
