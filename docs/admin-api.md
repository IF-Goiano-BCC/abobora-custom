# Admin API — bulk vote workflow

Base URL: `http://localhost:8080`

---

## 1. Login

```bash
curl -c cookies.txt -X POST http://localhost:8080/login \
  -d "username=admin&password=password"
```

`-c cookies.txt` saves the `session_id` cookie. All subsequent requests must include it with `-b cookies.txt`.

---

## 2. Get all participants

```bash
curl -b cookies.txt http://localhost:8080/admin/participants
```

**Response** — JSON array of cosplay entries:

```json
[
  {
    "ID": 1,
    "Nome": "Naruto",
    "Desc": "Classic Naruto costume",
    "Email": null,
    "Numero": null,
    "ImagePath": "https://..."
  },
  ...
]
```

`ImagePath` is a presigned URL valid for **2 hours**. Use the `ID` field when building votes.

---

## 3. Upload pre-computed votes

```bash
curl -b cookies.txt -X POST http://localhost:8080/admin/upload-votes \
  -H "Content-Type: application/json" \
  -d '[
    {
      "session_id": "batch-import-1",
      "cosplay_option1_id": 1,
      "cosplay_option2_id": 2,
      "cosplay_voted_id": 1
    },
    {
      "session_id": "batch-import-1",
      "cosplay_option1_id": 3,
      "cosplay_option2_id": 4,
      "cosplay_voted_id": 4,
      "timestamp": "2026-05-18T10:00:00Z"
    }
  ]'
```

**Fields:**

| Field | Required | Notes |
|---|---|---|
| `cosplay_option1_id` | yes | ID from participants list |
| `cosplay_option2_id` | yes | ID from participants list |
| `cosplay_voted_id` | yes | Must be one of the two option IDs |
| `session_id` | no | Defaults to empty string if omitted |
| `timestamp` | no | RFC 3339; defaults to server time if omitted |

**Response:**

```json
{"status":"ok","count":2}
```

---

## Full example (shell script)

```bash
BASE=http://localhost:8080

# 1. login
curl -sc cookies.txt -X POST $BASE/login \
  -d "username=admin&password=password" > /dev/null

# 2. fetch participants
curl -sb cookies.txt $BASE/admin/participants > participants.json

# 3. upload votes (edit votes.json first)
curl -sb cookies.txt -X POST $BASE/admin/upload-votes \
  -H "Content-Type: application/json" \
  -d @votes.json
```
