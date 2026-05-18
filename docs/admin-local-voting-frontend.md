# Admin local-voting frontend

A self-contained admin page that lets a judge vote offline (pairs stored in memory) and then push all results to the server in one shot.

---

## Flow

```
POST /api/login → store session_id → GET /admin/participants → build pairs locally → vote UI → POST /admin/upload-votes
```

1. Admin calls `POST /api/login` with JSON credentials and receives a `session_id`.
2. Every subsequent request includes `Authorization: Bearer <session_id>`.
3. Page fetches all participants and builds cosplay pairs client-side.
4. Judge goes through pairs and clicks a winner — votes are kept **only in a JS array**, nothing is sent yet.
5. After all pairs are judged, one button POSTs the full array to `/admin/upload-votes`.

---

## API contract

### Login
```
POST /api/login
Content-Type: application/json

{"username": "admin", "password": "password"}
```

Response:
```json
{ "session_id": "550e8400-e29b-41d4-a716-446655440000" }
```

Store `session_id` and pass it as `Authorization: Bearer <session_id>` on every subsequent request.

---

### Get participants
```
GET /admin/participants
Authorization: Bearer <session_id>
```
Returns JSON array. Presigned image URLs are valid for **2 hours** — build pairs and finish voting within that window.

```json
[
  { "ID": 1, "Nome": "Naruto",  "Desc": "...", "ImagePath": "https://..." },
  { "ID": 2, "Nome": "Goku",   "Desc": "...", "ImagePath": "https://..." },
  { "ID": 3, "Nome": "Luffy",  "Desc": "...", "ImagePath": "https://..." }
]
```

---

### Upload votes
```
POST /admin/upload-votes
Authorization: Bearer <session_id>
Content-Type: application/json

[
  {
    "session_id":        "judge-panel-1",
    "cosplay_option1_id": 1,
    "cosplay_option2_id": 2,
    "cosplay_voted_id":   1
  },
  ...
]
```

| Field | Required | Notes |
|---|---|---|
| `cosplay_option1_id` | yes | ID of the first cosplay in the pair |
| `cosplay_option2_id` | yes | ID of the second cosplay in the pair |
| `cosplay_voted_id` | yes | Must be one of the two option IDs |
| `session_id` | no | Use a fixed label (e.g. `"judge-panel-1"`) to identify the import batch |
| `timestamp` | no | RFC 3339; server uses current time if omitted |

Response:
```json
{ "status": "ok", "count": 6 }
```

No deduplication is applied — the server inserts every item as-is.

---

## Pair-building algorithm (client-side)

The same logic the server uses in `getRandomCosplayPairs`:

```js
function buildPairs(cosplays) {
  const shuffled = [...cosplays].sort(() => Math.random() - 0.5);
  const used = {};           // ID -> times used
  const maxRepeats = 1;
  const pairs = [];

  for (const a of shuffled) {
    if ((used[a.ID] ?? 0) >= maxRepeats) continue;
    for (const b of shuffled) {
      if (b.ID !== a.ID && (used[b.ID] ?? 0) < maxRepeats) {
        pairs.push([a, b]);
        used[a.ID] = (used[a.ID] ?? 0) + 1;
        used[b.ID] = (used[b.ID] ?? 0) + 1;
        break;
      }
    }
  }
  return pairs;
}
```

---

## Local vote accumulation

```js
const votes = [];   // filled as the judge clicks

function recordVote(pair, winnerId) {
  votes.push({
    session_id:         "judge-panel-1",
    cosplay_option1_id: pair[0].ID,
    cosplay_option2_id: pair[1].ID,
    cosplay_voted_id:   winnerId,
  });
}
```

Keep `votes` in memory. Optionally mirror it to `localStorage` so a page refresh doesn't lose progress:

```js
// save
localStorage.setItem("pending_votes", JSON.stringify(votes));

// restore on load
const saved = localStorage.getItem("pending_votes");
if (saved) votes.push(...JSON.parse(saved));
```

---

## Login

```js
let sessionId = null;

async function login(username, password) {
  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) throw new Error("Login failed");
  const data = await res.json();
  sessionId = data.session_id;
  // optional: persist across page refreshes
  sessionStorage.setItem("session_id", sessionId);
}

// restore on load
const saved = sessionStorage.getItem("session_id");
if (saved) sessionId = saved;
```

---

## Upload

```js
async function uploadVotes() {
  const res = await fetch("/admin/upload-votes", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${sessionId}`,
    },
    body: JSON.stringify(votes),
  });
  const data = await res.json();
  if (data.status === "ok") {
    localStorage.removeItem("pending_votes");
    alert(`Uploaded ${data.count} votes.`);
  }
}
```

---

## Minimal page skeleton

```html
<!DOCTYPE html>
<html>
<body>
  <div id="login-form">
    <input id="user" placeholder="username">
    <input id="pass" type="password" placeholder="password">
    <button onclick="doLogin()">Login</button>
  </div>
  <div id="pair" style="display:none">
    <img id="imgA"> <button onclick="vote('A')">Vote A</button>
    <img id="imgB"> <button onclick="vote('B')">Vote B</button>
  </div>
  <button id="upload-btn" onclick="uploadVotes()" style="display:none">
    Upload all votes
  </button>

  <script>
    let sessionId = sessionStorage.getItem("session_id");
    let pairs = [], current = 0, votes = [];

    async function doLogin() {
      const res = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username: document.getElementById("user").value,
          password: document.getElementById("pass").value,
        }),
      });
      if (!res.ok) { alert("Login failed"); return; }
      const data = await res.json();
      sessionId = data.session_id;
      sessionStorage.setItem("session_id", sessionId);
      document.getElementById("login-form").style.display = "none";
      init();
    }

    async function init() {
      const res = await fetch("/admin/participants", {
        headers: { "Authorization": `Bearer ${sessionId}` },
      });
      const data = await res.json();
      pairs = buildPairs(data);
      document.getElementById("pair").style.display = "block";
      showPair();
    }

    function showPair() {
      if (current >= pairs.length) {
        document.getElementById("pair").style.display = "none";
        document.getElementById("upload-btn").style.display = "block";
        return;
      }
      const [a, b] = pairs[current];
      document.getElementById("imgA").src = a.ImagePath;
      document.getElementById("imgB").src = b.ImagePath;
    }

    function vote(side) {
      const [a, b] = pairs[current];
      recordVote([a, b], side === "A" ? a.ID : b.ID);
      current++;
      showPair();
    }

    // paste buildPairs, recordVote, uploadVotes from above

    if (sessionId) init();
    else document.getElementById("login-form").style.display = "block";
  </script>
</body>
</html>
```

---

## Caveats

- **No server-side deduplication** — uploading twice adds duplicate rows. Clear `localStorage` and track whether an upload succeeded.
- **Presigned URLs expire in 2 hours** — reload participants if the session runs longer.
- **No vote-count enforcement** — the import bypasses `max_votes_per_session`; the server inserts everything sent.