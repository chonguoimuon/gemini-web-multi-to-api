<p align="center">
  <img src="assets/gemini.png" width="400" alt="Gemini Web-to-API Logo">
</p>

<p align="center">
  <a href="https://golang.org/"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go" alt="Go Version"></a>
  <a href="https://www.docker.com/"><img src="https://img.shields.io/badge/Docker-Self--Build-2496ED?style=flat-square&logo=docker" alt="Docker"></a>
  <a href="https://github.com/ntthanh2603/gemini-web-to-api/blob/main/LICENSE"><img src="https://img.shields.io/github/license/ntthanh2603/gemini-web-to-api?style=flat-square&color=orange" alt="License"></a>
  <img src="https://img.shields.io/badge/Maintained%3F-yes-green.svg?style=flat-square" alt="Maintained">
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-Native-blue.svg?style=flat-square" alt="MCP Native"></a>
</p>

<h1 align="center">🚀 Gemini Web-to-API & MCP Proxy</h1>

<p align="center">
  <b>Phiên bản 2026 | Quản lý đa tài khoản chuyên sâu</b><br/>
  Cấp độ Production với tích hợp MCP Native, Self-Healing Schema, và Anti-Bot Recovery
</p>

> [!IMPORTANT]
> **Phương thức Self-Build**: Dự án này không hỗ trợ image dựng sẵn. Mọi triển khai yêu cầu build trực tiếp từ mã nguồn để đảm bảo tính bảo mật, an toàn và tuân thủ chính sách.

---

## 📋 Mục Lục
1. [Công dụng chính](#-công-dụng-chính)
2. [Lợi ích nổi bật](#-lợi-ích-nổi-bật)
3. [Yêu cầu hệ thống](#-yêu-cầu-hệ-thống)
4. [Cài đặt & Cấu hình](#-cài-đặt--cấu-hình)
5. [Hướng dẫn chạy](#-hướng-dẫn-chạy)
6. [Truy cập trang điều khiển](#-truy-cập-trang-điều-khiển)
7. [Các thành phần quan trọng](#-các-thành-phần-quan-trọng)
8. [Xử lý sự cố](#-xử-lý-sự-cố)

---

## 📌 Công dụng chính

Dự án này đóng vai trò là **proxy reverse-engineered**, tương thích hoàn toàn với các giao diện API của OpenAI, Claude, và Gemini Native. Chủ yếu cung cấp các khả năng sau:

### 1. **Quản lý hệ thống đa tài khoản (Account Pool)**
   - Tự động **rotate** giữa các tài khoản Gemini để phân tán tải
   - Phát hiện và đề phòng Rate Limiting từ Google
   - Giám sát trạng thái sức khỏe từng tài khoản (Healthy/Cooldown/Error)
   - Cơ chế **pacing**: tự động giãn cách giữa các yêu cầu để mô phỏng hành vi người dùng thực

### 2. **Deep Research Mode (Nghiên cứu chuyên sâu)**
   - Thực hiện tìm kiếm web đa bước tự động
   - Tổng hợp thông tin từ nhiều nguồn
   - Phân tích logic chuyên sâu với khả năng suy luận cao

### 3. **Tích hợp MCP (Model Context Protocol)**
   - Cho phép các AI Agent (ví dụ: Antigravity) sử dụng nền tảng này như một bộ công cụ (tools)
   - Hỗ trợ SSE transport để giao tiếp real-time
   - Không cần API key từ Google - chỉ cần quản lý bằng `ADMIN_API_KEY` nội bộ

### 4. **API Compatibility Layers**
   - **OpenAI-compatible**: `/openai/v1/chat/completions`
   - **Claude API**: `/claude/v1/messages`
   - **Gemini API**: `/gemini/v1/chat/completions`
   - Cho phép thay thế endpoint một cách liền mạch

---

## ✨ Lợi ích nổi bật

| Lợi ích | Chi tiết | Ích lợi |
|---------|---------|--------|
| **Không cần API Key của Google** | Sử dụng session cookie từ trình duyệt | Tiết kiệm chi phí, tránh rate limit hệ thống |
| **Tự động phục hồi** | Self-healing schema detection khi Google thay đổi API | Không cần can thiệp thủ công, tính ổn định cao |
| **Phòng chống bot detection** | Cơ chế anti-bot recovery tự động | Tài khoản ít bị khóa, uptime liên tục |
| **Cân bằng tải thông minh** | Quản lý pool đa tài khoản với rotation | Vượt qua giới hạn rate limit |
| **Tích hợp MCP** | Hỗ trợ chuẩn Model Context Protocol | Kết nối với AI Agent, tự động hóa phức tạp |
| **Tương thích API** | Giả lập OpenAI, Claude, Gemini | Dùng làm drop-in replacement cho các công cụ hiện tại |
| **Mã nguồn mở** | Self-build, không phụ thuộc registry | Toàn quyền kiểm soát, bảo mật cao |

---

## 💻 Yêu cầu hệ thống

### Tối thiểu
- **CPU**: 2 cores
- **RAM**: 2GB (tối thiểu), **4GB trở lên khuyến khích** (để chạy trình duyệt anti-bot)
- **Disk**: 1GB cho mã nguồn + các dependencies

### Khuyên dùng (Production)
- **OS**: Linux (Ubuntu 20.04+, Alpine 3.22+) hoặc Windows Server
- **Docker**: 20.10+
- **Go**: 1.25.1+ (nếu build từ source trực tiếp)
- **RAM**: 4-8GB (để chạy multiple browser instances khi cần anti-bot recovery)

### Bộ nhớ browser
- **Chromium headless**: ~300-500MB mỗi instance
- **Sequential Browser Queue**: Chỉ chạy 1 instance cùng lúc để tránh tràn bộ nhớ

---

## ⚙️ Cài đặt & Cấu hình

### **Bước 1: Clone Repository**

```bash
git clone https://github.com/chonguoimuon/gemini-web-multi-to-api.git
cd gemini-web-multi-to-api
```

### **Bước 2: Tạo file cấu hình (.env)**

```bash
cp .env.example .env
```

Chỉnh sửa `.env` với các giá trị quan trọng:

```dotenv
# ========== CỔNG & LOGGING ==========
PORT=4982
LOG_LEVEL=info

# ========== ADMIN (Bắt buộc) ==========
ADMIN_API_KEY=your-super-secret-key-here-min-32-chars

# ========= PHƯƠNG ÁN 1: Gemini Web Với Session Cookie (Tùy chọn) =========
# Nếu bạn muốn sử dụng tài khoản Gemini Web đã đăng nhập
# Cách lấy: Xem hướng dẫn chi tiết bên dưới
GEMINI_1PSID=your_1psid_cookie_value
GEMINI_1PSIDTS=your_1psidts_cookie_value

# ========= PHƯƠNG ÁN 2: Gemini Guest (Không cần Cookie) =========
# Phương án dự phòng cho chat cơ bản - không cần cookie user
# Đơn giản hóa setup, nhưng có giới hạn rate limit Google
# Để trống nếu muốn sử dụng Phương ÁN 1
GEMINI_GUEST_MODE=false

# ========== SELF-HEALING ORACLE: API Key Phục Hồi Schema (Tùy chọn) ==========
# API Keys từ Google Gemini API Console (generativelanguage.googleapis.com)
# Format: JSON array - có thể cấu hình 1 hoặc nhiều keys
# Mục đích: Khi Google thay đổi cấu trúc JSPB, hệ thống sẽ auto-recovery
# Hướng dẫn:
#   1. Truy cập: https://aistudio.google.com/apikey
#   2. Tạo API Key mới (hoặc copy key có sẵn)
#   3. Paste dưới dạng JSON array
# CÓ THỂ ĐỂ TRỐNG [] nếu không cần auto-recovery
GEMINI_PRO_API_KEYS=["your_api_key_1", "your_api_key_2"]

# ========== QUẢN LÝ TÀI KHOẢN ==========
GEMINI_REFRESH_INTERVAL=1440  # Phút | Tần suất làm mới phiên đăng nhập

# ========== RATE LIMITING ==========
RATE_LIMIT_ENABLED=true
RATE_LIMIT_WINDOW_MS=60000      # Cửa sổ thời gian (ms)
RATE_LIMIT_MAX_REQUESTS=30      # Số request tối đa trong cửa sổ

# ========== MCP PROTOCOL ==========
MCP_ENABLED=true

# ========== HỖ TRỢ DEEP RESEARCH ==========
DEEP_RESEARCH_ENABLED=true
DEEP_RESEARCH_MAX_STEPS=5       # Bước tìm kiếm tối đa
```

### **Bước 3: Chọn Phương Án Setup**

#### **🟢 Lựa chọn A: Gemini Web (Recommended - Không cần chi phí API)**

**Ưu điểm:**
- ✅ Không cần API Key từ Google
- ✅ Lượng request unlimited (miễn là cookie còn hiệu lực)
- ✅ Full features: Deep Research, chat advanced, v.v.

**Hướng dẫn lấy Cookie:**

1. Truy cập [gemini.google.com](https://gemini.google.com) và đăng nhập bằng tài khoản Google
2. Nhấn `F12` để mở Developer Tools
3. Đi tới tab **Application** (hoặc **Storage**)
4. Chọn **Cookies** → `https://gemini.google.com`
5. Tìm và copy giá trị của:
   - `__Secure-1PSID` → dán vào `GEMINI_1PSID`
   - `__Secure-1PSIDTS` → dán vào `GEMINI_1PSIDTS`

![Hướng dẫn lấy cookie](assets/gemini_token_guide.png)

> 💡 **Mẹo**: Nếu sử dụng nhiều tài khoản, có thể thêm vào file `data/accounts.json` sau khi cấu hình lần đầu

---

#### **🟡 Lựa chọn B: Gemini Guest (Backup không cần thiết lập)**

**Khi nào dùng:**
- ✅ Muốn chạy nhanh không cần setup cookie
- ✅ Cần phương án dự phòng khi Gemini Web bị cấm
- ✅ Chat cơ bản, không cần Deep Research

**Ưu / Nhược điểm:**
| | Guest Mode | Gemini Web |
|---|-----------|-----------|
| **Setup** | Không cần cookie | Cần lấy cookie |
| **Chat cơ bản** | ✅ Có | ✅ Có |
| **Deep Research** | ❌ Không | ✅ Có |
| **Rate Limit** | Google quản lý (~ 50 req/giờ) | Unlimited |
| **Chi phí** | Miễn phí | Miễn phí |

**Cách kích hoạt:**
```dotenv
GEMINI_GUEST_MODE=true
# Để GEMINI_1PSID & GEMINI_1PSIDTS trống
GEMINI_1PSID=
GEMINI_1PSIDTS=
```

---

### **Bước 4: Cấu hình Self-Healing Oracle (Optional)**

Nếu bạn muốn hệ thống **tự động phục hồi** khi Google thay đổi API:

**Yêu cầu:**
- 1 hoặc nhiều Gemini API Keys từ [Google AI Studio](https://aistudio.google.com/apikey)

**Cách cấu hình (JSON format):**
```dotenv
# 1 key
GEMINI_PRO_API_KEYS=["AIzaSyDw8GJt2eXBnmqxWJBT-xxxxxxxxxxxxxx"]

# Nhiều keys (auto-rotate)
GEMINI_PRO_API_KEYS=["key1", "key2", "key3"]

# Để trống nếu không cần
GEMINI_PRO_API_KEYS=[]
```

> 💡 **Lợi ích**: Khi schema thay đổi, hệ thống sẽ tự động gọi API này để tìm ra cấu trúc mới - **không cần can thiệp thủ công**

---

## ⚡ Quick Start (Chạy nhanh)

Muốn chạy ngay mà không cần lấy cookie? Sử dụng **Gemini Guest Mode**:

```bash
# 1. Clone & setup
git clone https://github.com/chonguoimuon/gemini-web-multi-to-api.git
cd gemini-web-multi-to-api
cp .env.example .env

# 2. Config .env (chỉ cần 2 dòng)
echo "PORT=4982" >> .env
echo "ADMIN_API_KEY=my-secret-key-123456789" >> .env
echo "GEMINI_GUEST_MODE=true" >> .env

# 3. Chạy trực tiếp bằng Go (Nhanh nhất)
go run cmd/server/main.go

# Nếu muốn xem log chi tiết (Debug)
LOG_LEVEL=debug go run cmd/server/main.go

# Hoặc dùng Docker Compose (Recommended)
docker compose up -d --build
```

**Kết quả:**
- ✅ Server sẽ chạy tại `http://localhost:4982`
- ✅ Có thể chat cơ bản ngay lập tức
- ✅ Truy cập Dashboard tại `http://localhost:4982/docs`

> 💡 Sau này, nếu muốn full features (Deep Research, unlimited requests), bạn chỉ cần thêm `GEMINI_1PSID` & `GEMINI_1PSIDTS` vào .env

---

## 🎯 Hướng dẫn chạy

### **Phương pháp 1: Chạy trực tiếp bằng Go** (Dành cho lập trình viên)

**Yêu cầu**: Go 1.25.1 trở lên

```bash
# 1. Cài đặt dependencies
go mod download

# 2. Cấu hình .env như hướng dẫn trên

# 3. Chạy trực tiếp
go run cmd/server/main.go

# Hoặc chạy development mode với auto-reload
task dev        # Nếu cài đặt Task runner

# Hoặc build binary trước
go build -o gemini-web-to-api cmd/server/main.go
./gemini-web-to-api
```

**Output mong đợi:**
```
INFO    server listening at 0.0.0.0:4982
INFO    Swagger documentation: http://localhost:4982/swagger/index.html
INFO    Scalar Dashboard: http://localhost:4982/docs
INFO    MCP Server enabled on /mcp/sse
```

**Liên tục kiểm tra logs:**
```bash
# Nếu muốn xem log chi tiết
LOG_LEVEL=debug go run cmd/server/main.go
```

---

### **Phương pháp 2: Chạy bằng Docker Compose** (Khuyến nghị cho Production)

**Yêu cầu**: Docker & Docker Compose

```bash
# 1. Tạo file .env (nếu chưa có)
cp .env.example .env

# 2. Sửa đổi .env với thông tin tài khoản

# 3. Build image từ mã nguồn hiện tại
docker compose up -d --build

# 4. Kiểm tra logs
docker compose logs -f gemini-api

# 5. Dừng dịch vụ khi cần
docker compose down
```

**Ưu điểm Docker Compose:**
- ✅ Tự động build image từ Dockerfile
- ✅ Quản lý volumes cho dữ liệu persistent
- ✅ Dễ scale hoặc chạy multiple instances
- ✅ Tự động khởi động lại khi crash
- ✅ Kích hoạt Chromium cho anti-bot recovery (có sẵn trong Alpine container)

**Kiểm tra trạng thái container:**
```bash
docker compose ps
docker compose logs --tail=50 gemini-api
```

---

### **Phương pháp 3: Build và Chạy Standalone Docker**

```bash
# Build image
docker build -t gemini-web-to-api:latest .

# Chạy container
docker run -d \
  --name gemini-api \
  -p 4982:4982 \
  -v $(pwd)/data:/app/data \
  -v $(pwd)/.env:/app/.env \
  --memory=2g \
  gemini-web-to-api:latest

# Check logs
docker logs -f gemini-api
```

---

## 🎛️ Truy cập trang điều khiển

Sau khi khởi động thành công, bạn có thể truy cập các giao diện quản lý:

### **1. Dashboard Scalar** (Khuyến nghị - Giao diện hiện đại)
```
URL: http://localhost:4982/docs
```
- ✅ Giao diện trực quan, dễ sử dụng
- ✅ Kiểm tra danh sách API endpoints
- ✅ Test API trực tiếp trong giao diện
- ✅ Xem trạng thái tài khoản, MCP tools

### **2. Swagger UI** (Chuẩn OpenAPI)
```
URL: http://localhost:4982/swagger/index.html
```
- ✅ Tài liệu API đầy đủ
- ✅ Hỗ trợ tất cả các endpoint

### **3. Admin Dashboard** (Quản lý tài khoản)
```
URL: http://localhost:4982/admin
Authentication: Dùng ADMIN_API_KEY trong header:
Header: Authorization: Bearer <YOUR_ADMIN_API_KEY>
```

Các chức năng:
- Xem danh sách tài khoản & trạng thái (Healthy/Banned/Cooldown)
- Thêm/xóa tài khoản mà không cần restart
- Kiểm tra metrics & rate limits
- Trigger manual account refresh

---

## 🔧 Các thành phần quan trọng

### **1. Account Pool - Quản lý đa tài khoản**

**Cách hoạt động:**
- Mỗi yêu cầu sẽ được gán cho một tài khoản khỏe mạnh từ pool
- Nếu tài khoản gặp rate limit, hệ thống tự động:
  - Chuyển sang tài khoản khác
  - Đưa tài khoản hiện tại vào "cooldown" (nghỉ 2-5 phút)
  - Ghi log chi tiết cho việc debug

**File lưu trữ:**
- `data/accounts.json` - Lưu thông tin & trạng thái tất cả tài khoản

---

### **2. Gemini Guest Mode - Phương án dự phòng không cần Setup**

**Khái niệm:**
- Một phương án thay thế khi muốn chat cơ bản mà **không cần lấy cookie từ Gemini Web**
- Sử dụng Google's public Gemini guest endpoint - không yêu cầu xác thực
- Tự động fallback khi Gemini Web Pool bị lỗi hoặc đạt rate limit

**Mục đích sử dụng:**
- ✅ **Phương án dự phòng**: Khi tất cả tài khoản Gemini Web bị khóa, Guest Mode vẫn hoạt động
- ✅ **Setup nhanh**: Không cần tìm cookie, chỉ enable flag và chạy
- ✅ **Chat cơ bản**: Đáp ứng nhu cầu conversation đơn giản
- ✅ **Kiểm tra hệ thống**: Dùng để test API endpoints mà không cần tài khoản

**Giới hạn:**
| Tính năng | Guest Mode | Gemini Web |
|----------|-----------|-----------|
| Chat cơ bản | ✅ | ✅ |
| Extended thinking | ✅ | ✅ |
| Deep Research | ❌ | ✅ |
| File uploads | ❌ | ✅ |
| Custom instructions | ❌ | ✅ |
| Rate limit | ~50 req/giờ | Unlimited |

**Cách kích hoạt:**
```bash
# Trong .env
GEMINI_GUEST_MODE=true

# Hoặc khi chạy (override)
GEMINI_GUEST_MODE=true PORT=4982 go run cmd/server/main.go
```

**Cơ chế fallback:**
```
Request đến /chat/completions
  ↓
[Priority 1] Kiểm tra Gemini Web Pool (nếu available)
  ↓ (Pool bị lỗi / empty)
[Priority 2] Fallback sang Guest Mode
  ↓
Response từ Gemini Guest (nếu GEMINI_GUEST_MODE=true)
```

**File cấu hình:**
- `data/guest_platforms.json` - Lưu trạng thái & endpoints của Guest Mode

---

### **3. Self-Healing Schema Detection**

**Vấn đề giải quyết:**
- Google thường xuyên thay đổi cấu trúc response JSPB payload
- Khi cấu trúc thay đổi, các công cụ khác sẽ bị lỗi trích xuất dữ liệu

**Giải pháp:**
- Khi phát hiện extraction failed (mặc dù HTTP 200), hệ thống sẽ:
  1. Gửi payload minified đến "Oracle" (`GEMINI_PRO_API_KEYS`)
  2. Oracle phân tích và trả về cấu trúc extraction path mới (dùng GJSON)
  3. Xác nhận phục hồi thành công bằng cách gửi probe message `"hãy trả lời 'ok'"`
  4. Lưu schema mới vào cache để sử dụng sau

**Tệp cấu hình:**
- `data/gemini_schema.json` - Schema extraction paths

---

### **4. Anti-Bot Recovery (Xử lý khóa tài khoản)**

**Khi nào kích hoạt:**
- Nhận HTTP 429 với nội dung HTML (không phải JSON)
- Nhận HTTP 403 Forbidden

**Quá trình phục hồi:**
1. Tài khoản được đánh dấu trạng thái `Banned`
2. **Mở trình duyệt headless Chromium** (Rod library) tự động
   - 💡 **Tại sao**: Mô phỏng hành vi người dùng thực để vượt qua kiểm tra bot của Google
3. Thực hiện hành động mô phỏng (mouse movement, click, scroll)
4. Gửi message probe: `"hãy trả lời 'ok'"`
5. Chờ tối đa 60 giây để Google xác nhận
6. Lưu ảnh debug `debug_clear_bot.png` cho tham khảo
7. Đóng trình duyệt, chuyển sang tài khoản tiếp theo

**Lưu ý quan trọng:**
- ⚠️ **Chỉ chạy 1 trình duyệt cùng lúc** (sequential queue) để tránh tràn bộ nhớ
- ⚠️ **Yêu cầu bộ nhớ**: Tối thiểu 300-500MB mỗi instance
- Browser profiles được cache tại `data/browser_profiles/{account_id}/`

---

### **5. Pacing & Cooldown - Mô phỏng hành vi người dùng**

**Pacing Delay:** Tự động chờ ~1.5 giây giữa các step Research
- Mục đích: Tránh bị Google phát hiện là bot (quá nhanh = bot)

**Account Cooldown:** Khi gặp lỗi, tài khoản sẽ "nghỉ" 2-5 phút
- Mục đích: Google có thời gian "lạnh đầu", sau đó cho phép kết nối lại

**Heartbeat Connection:** Duy trì phiên SSE liên tục
- Mục đích: Tránh Cloudflare ngắt tunnel khi yêu cầu lâu

---

### **6. MCP Protocol (Tích hợp AI Agent)**

MCP là chuẩn giao tiếp giữa các AI Agent và các tool bên ngoài.

**Cách tích hợp:**

```json
// mcp_config.json trong Antigravity hoặc AI Agent của bạn
{
  "mcpServers": {
    "gemini-web-multi": {
      "serverUrl": "http://localhost:4982/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_ADMIN_API_KEY"
      }
    }
  }
}
```

**Kết quả:**
- AI Agent sẽ nhận được danh sách tools từ `gemini-web-multi`
- Có thể gọi các tools như: `deep-search`, `chat-completion`, `get-account-status`
- Tất cả được thực thi trên backend pool tài khoản của bạn

---

## 🔌 Tích hợp API

### **OpenAI-Compatible Endpoint**
```bash
curl -X POST http://localhost:4982/openai/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-advanced",
    "messages": [{"role": "user", "content": "Xin chào"}]
  }'
```

### **Claude API Endpoint**
```bash
curl -X POST http://localhost:4982/claude/v1/messages \
  -H "Authorization: Bearer YOUR_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-sonnet",
    "messages": [{"role": "user", "content": "Xin chào"}]
  }'
```

### **Direct Gemini Endpoint**
```bash
curl -X POST http://localhost:4982/gemini/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-pro",
    "messages": [{"role": "user", "content": "Xin chào"}]
  }'
```

---

## 🛡️ Bảo mật & Tuân thủ chính sách

### **Bảng so sánh: Khi nào dùng cái gì?**

| Trường hợp | Giải pháp | Cách config | Thích hợp |
|-----------|---------|-----------|----------|
| **Muốn chạy ngay không setup** | Guest Mode | `GEMINI_GUEST_MODE=true` | ✅ Prototype, test |
| **Cần full features + unlimited** | Gemini Web Pool | `GEMINI_1PSID` + `GEMINI_1PSIDTS` | ✅ Production |
| **Cần auto-recovery schema** | Oracle (Pro API Key) | `GEMINI_PRO_API_KEYS=["key1"]` | ✅ 24/7 stability |
| **Chỉ access dashboard/admin** | Admin API Key | `ADMIN_API_KEY` | ✅ Bắt buộc |

---

### **Chi tiết các loại Key/Cookie**

**1. Gemini Web Cookies - `GEMINI_1PSID` & `GEMINI_1PSIDTS`** (Tuỳ chọn, nhưng **Recommended**)
- 🔐 **Là gì**: Thanh toán xác thực phiên làm việc hợp lệ trên Gemini Web
- 📌 **Được dùng cho**: Chat cơ bản, Deep Research, Extended thinking
- ✅ **Lợi ích**: Unlimited requests (không rate limit API), full features
- ✅ **Tuân thủ**: Mỗi request là hành động do người dùng thực hiện (không blind scraping)
- ⚠️ **Lưu ý**: 
  - Không chia sẻ công khai, giữ bí mật
  - Refresh mỗi 24 giờ (hệ thống tự động)
  - 1 cookie = 1 tài khoản

**2. Gemini Guest Mode** (Không cần Key)
- 🔐 **Là gì**: Public endpoint của Google, không yêu cầu xác thực
- 📌 **Được dùng cho**: Chat cơ bản, phương án dự phòng
- ✅ **Lợi ích**: 
  - Setup nhanh, không cần cookie
  - Fallback tự động khi Gemini Web Pool lỗi
  - Không cần giữ secret
- ⚠️ **Giới hạn**:
  - Rate limit ~50 requests/giờ
  - Không có Deep Research
  - Kích hoạt: `GEMINI_GUEST_MODE=true`

**3. Gemini Pro API Key** (Tùy chọn, cho Schema Recovery)
- 🔐 **Là gì**: API Key chính thức từ Google AI Studio
- 📌 **Được dùng cho**: Self-healing Oracle (tự phục hồi schema khi thay đổi)
- ✅ **Lợi ích**: Hệ thống tự động phát hiện & khắc phục lỗi API thay đổi
- ⚠️ **Lưu ý**:
  - Để trống `GEMINI_PRO_API_KEYS=[]` nếu không cần
  - Chỉ gọi khi schema thay đổi (không phải request thực tế)
  - Format: JSON array `["key1", "key2", "key3"]`
  - Có quota giới hạn trên Google Console
  
**Cách lấy Gemini Pro API Key:**
```bash
1. Truy cập: https://aistudio.google.com/apikey
2. Click "Create API Key" hoặc copy key có sẵn
3. Paste vào .env theo format JSON:
   GEMINI_PRO_API_KEYS=["AIzaSyDw..."]
```

**4. Admin API Key** (Bắt buộc)
- 🔐 **Là gì**: Mật khẩu nội bộ của hệ thống
- 📌 **Được dùng cho**: 
  - Xác thực request từ Dashboard
  - Xác thực MCP connection từ Antigravity
  - Admin endpoints (quản lý tài khoản)
- ✅ **Tính năng**:
  - Quản lý pool tài khoản
  - Xem metrics & status
  - Trigger manual recovery
- ⚠️ **Lưu ý**: 
  - Giữ bí mật, như password
  - Tối thiểu 32 ký tự khuyên dùng
  - Format: Plain text (không JSON)
  - Header: `Authorization: Bearer YOUR_ADMIN_API_KEY`

---

### **Tuân thủ chính sách Google**

Dự án này được thiết kế để:
- ✅ **Hoạt động hợp pháp**: Sử dụng session được phép (không hack/spoof)
- ✅ **Không scraping blind**: Mỗi request là hành động đã xác nhân từ người dùng
- ✅ **Tránh phát hiện bot**: 
  - Pacing delay giữa requests
  - Mô phỏng user-agent, headers hợp nhất
  - Sequential browser queue để tránh resource exhaustion
- ✅ **Tự động phục hồi**: Anti-bot recovery khi gặp 429/403
- ⚠️ **Bất khả chối**: Dự án này là **reverse-engineered**, cần tuân thủ chính sách ToS của Google

---

## 🐛 Xử lý sự cố

### **Tài khoản bị khóa thường xuyên**

**Triệu chứng**: Status `Banned` hoặc liên tục nhận 429

**Nguyên nhân**: 
- Pacing quá nhanh
- Rate limit từ Google
- Phát hiện hoạt động anormal

**Giải pháp**:
```bash
# 1. Xem log chi tiết
docker compose logs -f gemini-api | grep ERROR

# 2. Tăng RATE_LIMIT_WINDOW_MS hoặc giảm RATE_LIMIT_MAX_REQUESTS
# trong .env

# 3. Thêm tài khoản mới vào pool
curl -X POST http://localhost:4982/admin/accounts/add \
  -H "Authorization: Bearer YOUR_ADMIN_API_KEY" \
  -d '{"1psid": "new_cookie_1", "1psidts": "new_cookie_2"}'

# 4. Chờ cooldown tự động (2-5 phút)
```

---

### **Schema Healing không hoạt động**

**Triệu chứng**: Log hiện `🚨 EXTRACTION FAILED`, nhưng không phục hồi được

**Nguyên nhân**:
- `GEMINI_PRO_API_KEYS` không hợp lệ hoặc bị rate limit
- Oracle đang bị block

**Giải pháp**:
```bash
# 1. Kiểm tra API key hợp lệ
curl https://generativelanguage.googleapis.com/v1/models/gemini-pro:generateContent \
  -X POST -d '{"contents":[{"parts":[{"text":"test"}]}]}' \
  -H "x-goog-api-key: YOUR_KEY"

# 2. Nếu không còn slot, để trống GEMINI_PRO_API_KEYS
# Hệ thống sẽ dùng fallback mode (chỉ retry cũ)

# 3. Restart service
docker compose restart
```

---

### **Chromium không khởi động (Anti-bot fails)**

**Triệu chứng**: Log hiện `ClearBot: failed to spawn browser`

**Nguyên nhân**:
- Bộ nhớ không đủ
- Chromium không cài đặt trong Docker
- File permission issue

**Giải pháp**:
```bash
# 1. Tăng memory limit
docker compose down
# Chỉnh sửa docker-compose.yml: mem_limit: 4g
docker compose up -d

# 2. Kiểm tra Chromium có cài không
docker compose exec gemini-api which chromium-browser

# 3. Xem log chi tiết
docker compose logs gemini-api | grep -i chromium
```

---

### **MCP endpoint không phản hồi**

**Triệu chứng**: Kết nối SSE `/mcp/sse` timeout

**Nguyên nhân**:
- Tài khoản trong pool không khỏe mạnh
- MCP_ENABLED=false trong .env

**Giải pháp**:
```bash
# 1. Kiểm tra MCP enabled
grep MCP_ENABLED .env

# 2. Xác thực Authorization header
curl http://localhost:4982/mcp/sse \
  -H "Authorization: Bearer YOUR_ADMIN_API_KEY"

# 3. Xem logs
docker compose logs -f | grep MCP
```

---

## 📊 Monitoring & Metrics

Các endpoint quan trọng để monitoring:

```bash
# Xem trạng thái tài khoản
GET /admin/accounts/status
Authorization: Bearer YOUR_ADMIN_API_KEY

# Xem API usage metrics
GET /admin/metrics
Authorization: Bearer YOUR_ADMIN_API_KEY

# Xem MCP tools
GET /mcp/api/tools
Authorization: Bearer YOUR_ADMIN_API_KEY
```

---

## 📚 Tài liệu bổ sung

- **[ARCHITECTURE_SELF_HEALING.md](./ARCHITECTURE_SELF_HEALING.md)**: Chi tiết về cơ chế tự phục hồi schema
- **[.env.example](./.env.example)**: Mẫu cầu hình đầy đủ
- **[docker-compose.yml](./docker-compose.yml)**: Cấu hình Docker Compose
- **[Swagger API Docs](http://localhost:4982/swagger/index.html)**: Tài liệu API live

---

## 📄 Giấy phép & Tuân thủ

- **License**: MIT
- **Disclaimer**: Dự án này được cung cấp như hiện tại. Người dùng chịu trách nhiệm tuân thủ Điều khoản Dịch vụ của Google
- **Mục đích**: Nghiên cứu, học tập, và tự động hóa cá nhân
- **Không hỗ trợ**: Scraping hàng loạt, spam, hoặc hoạt động vi phạm ToS

---

<p align="center">
Made with ❤️ for automation enthusiasts & researchers
</p>