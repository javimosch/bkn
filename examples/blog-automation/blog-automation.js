// Blog automation: pick a topic, research it, draft a post, illustrate it,
// publish it.
//
// Replaces blogAutomationRun/Config/Publishing plus the llm services from the
// Node backend (~2,500 lines across services, controllers, routes and three
// Mongoose models) with one script driven by cron.
//
// Install:
//   bkn kv set blog.llm_key sk-... --type encrypted
//   bkn store put blog/configs --id default --data @config.json
//   bkn files ns create blog-images --allow-type 'image/*' --public
//   bkn script create blog-automation --file blog-automation.js \
//     --allow-net api.openai.com --timeout 180000
//   bkn cron create blog-daily --schedule '0 7 * * *' --script blog-automation

const LOCK_KEY = "blog-automation";
const LOCK_TTL_SECONDS = 900;

// --- LLM ------------------------------------------------------------------

function chat(cfg, spec) {
  const key = bkn.kv.get(cfg.provider.api_key_setting);
  if (!key) throw new Error("missing LLM key in setting " + cfg.provider.api_key_setting);

  const res = bkn.http.fetch(cfg.provider.base_url + "/chat/completions", {
    method: "POST",
    headers: { "Authorization": "Bearer " + key, "Content-Type": "application/json" },
    timeout_ms: spec.timeout_ms || 120000,
    body: {
      model: spec.model || cfg.provider.model,
      temperature: spec.temperature,
      max_tokens: spec.max_tokens,
      messages: [
        { role: "system", content: spec.system },
        { role: "user", content: spec.user },
      ],
    },
  });
  if (!res.ok) throw new Error("LLM returned HTTP " + res.status + ": " + res.body.slice(0, 300));

  const choice = res.json && res.json.choices && res.json.choices[0];
  const content = choice && choice.message && choice.message.content;
  if (!content) throw new Error("LLM returned no content");
  return content;
}

// Models are asked for JSON and sometimes wrap it in a markdown fence anyway.
function parseLooseJson(text) {
  const trimmed = String(text).trim().replace(/^```(?:json)?/, "").replace(/```$/, "").trim();
  try {
    return JSON.parse(trimmed);
  } catch (e) {
    const start = trimmed.indexOf("{");
    const end = trimmed.lastIndexOf("}");
    if (start >= 0 && end > start) return JSON.parse(trimmed.slice(start, end + 1));
    throw new Error("model did not return JSON: " + trimmed.slice(0, 200));
  }
}

// --- helpers --------------------------------------------------------------

function pickWeightedTopic(topics) {
  const pool = (topics || []).filter((t) => t && t.key);
  if (pool.length === 0) throw new Error("config has no topics");
  const total = pool.reduce((sum, t) => sum + (Number(t.weight) || 1), 0);
  let roll = Math.random() * total;
  for (const topic of pool) {
    roll -= Number(topic.weight) || 1;
    if (roll <= 0) return topic;
  }
  return pool[pool.length - 1];
}

function slugify(title) {
  return String(title).toLowerCase()
    .replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 80) || "post";
}

// Slugs must be unique; the store has no unique index beyond the id, so the
// slug IS the id and putIfAbsent decides collisions.
function claimSlug(base) {
  for (let attempt = 0; attempt < 20; attempt++) {
    const slug = attempt === 0 ? base : base + "-" + (attempt + 1);
    if (bkn.store.get("blog/posts", slug) === null) return slug;
  }
  return base + "-" + bkn.id().toLowerCase().slice(-6);
}

function runsToday() {
  const since = new Date(Date.now() - 24 * 3600 * 1000).toISOString().replace(/\.\d+Z$/, "Z");
  return bkn.events.list("blog", { type: "run.published", since: since, limit: 100 }).length;
}

function record(runId, status, extra) {
  bkn.store.patch("blog/runs", runId, Object.assign({ status: status, ended_at: bkn.now() }, extra || {}));
  return Object.assign({ run: runId, status: status }, extra || {});
}

// --- image ----------------------------------------------------------------

// The image model is asked for a data URL rather than a hosted link: a URL
// would need a second fetch, and http.fetch would have to be told the response
// is binary. A data URL arrives already base64-encoded.
const DATA_URL = /^data:(image\/[a-zA-Z0-9.+-]+);base64,(.+)$/;

function generateCover(cfg, title, slug) {
  const answer = chat(cfg, {
    model: cfg.images.model,
    system: "You produce a single image as a data URL. Return ONLY a string like " +
            "data:image/png;base64,... with no prose and no markdown fence.",
    user: "A tasteful, text-free editorial cover image for an article titled: " + title +
          (cfg.images.prompt_extra ? "\n" + cfg.images.prompt_extra : ""),
    max_tokens: cfg.images.max_tokens || 4000,
  });
  const match = DATA_URL.exec(String(answer).trim());
  if (!match) throw new Error("image model did not return a data URL");

  const file = bkn.files.put(cfg.images.namespace, slug + "." + match[1].split("/")[1], match[2], {
    encoding: "base64", contentType: match[1], overwrite: true,
    metadata: { slug: slug, generated: true },
  });
  return { name: file.name, namespace: file.namespace, size: file.size, content_type: file.content_type };
}

// --- main -----------------------------------------------------------------

function main(input) {
  const configId = (input && input.config) || "default";
  const trigger = (input && input.trigger) || "manual";

  const cfg = bkn.store.get("blog/configs", configId);
  if (!cfg) throw new Error("no blog config " + configId);
  if (!cfg.enabled) return { skipped: "config disabled", config: configId };

  if (trigger === "scheduled" && cfg.runs_per_day_limit > 0) {
    const already = runsToday();
    if (already >= cfg.runs_per_day_limit) {
      return { skipped: "daily limit reached", published_today: already };
    }
  }

  // Manual and scheduled runs can collide, and so can two hosts sharing one
  // database. The lease is what makes that safe.
  const lock = bkn.lock.acquire(LOCK_KEY, LOCK_TTL_SECONDS);
  if (!lock) return { skipped: "another run holds the lock" };

  const runId = bkn.id();
  bkn.store.put("blog/runs", {
    config: configId, trigger: trigger, status: "running",
    started_at: bkn.now(), steps: [],
  }, runId);

  const steps = [];
  const step = (name, data) => {
    steps.push({ step: name, at: bkn.now(), data: data });
    bkn.store.patch("blog/runs", runId, { steps: steps });
  };

  try {
    const topic = pickWeightedTopic(cfg.topics);
    step("topic", { key: topic.key, label: topic.label });

    const idea = parseLooseJson(chat(cfg, {
      model: cfg.text.model, temperature: cfg.text.temperature, max_tokens: 600,
      system: "Return ONLY valid JSON. No markdown fences.",
      user: "Given the theme below, propose one specific article angle and a single web " +
            "research query.\nTheme: " + (topic.label || topic.key) +
            '\nReturn: {"title": string, "angle": string, "research_query": string}',
    }));
    step("idea", idea);

    const research = parseLooseJson(chat(cfg, {
      model: cfg.research.model, temperature: cfg.research.temperature,
      max_tokens: cfg.research.max_tokens,
      system: "Perform web research and return ONLY valid JSON. Include citations as sources[].",
      user: "Collect up-to-date information for this query and return structured research.\n" +
            "Query: " + idea.research_query +
            '\nReturn: {"summary": string, "facts": string[], "sources": [{"title": string, "url": string}]}',
    }));
    step("research", { facts: (research.facts || []).length, sources: (research.sources || []).length });

    const markdown = chat(cfg, {
      model: cfg.text.model, temperature: cfg.text.temperature, max_tokens: cfg.text.max_tokens,
      system: "You write publication-ready articles in Markdown. Return ONLY the article body, " +
              "no front matter and no commentary." + (cfg.style_guide ? "\nStyle guide:\n" + cfg.style_guide : ""),
      user: "Title: " + idea.title + "\nAngle: " + idea.angle +
            "\n\nResearch summary:\n" + (research.summary || "") +
            "\n\nFacts:\n" + (research.facts || []).map((f) => "- " + f).join("\n") +
            "\n\nCite the sources inline where relevant:\n" +
            (research.sources || []).map((s) => "- " + s.title + " (" + s.url + ")").join("\n"),
    });
    step("draft", { characters: markdown.length });

    const slug = claimSlug(slugify(idea.title));
    let cover = null;
    if (cfg.images && cfg.images.enabled) {
      try {
        cover = generateCover(cfg, idea.title, slug);
        step("cover", cover);
      } catch (err) {
        // A missing illustration is not worth losing the article over.
        step("cover_error", { error: String(err && err.message || err) });
        bkn.events.emit("blog", "image.failed", {
          level: "warn", subject: slug, data: { error: String(err && err.message || err) },
        });
      }
    }

    if (cfg.dry_run) {
      bkn.lock.release(lock.key, lock.owner);
      return record(runId, "dry_run", { slug: slug, title: idea.title, characters: markdown.length });
    }

    const post = bkn.store.put("blog/posts", {
      title: idea.title, slug: slug, topic: topic.key, angle: idea.angle,
      body_markdown: markdown, sources: research.sources || [],
      cover: cover, status: cfg.publish_immediately ? "published" : "draft",
      generated_by: "blog-automation", run: runId, published_at: bkn.now(),
    }, slug);
    step("published", { slug: post.slug });

    bkn.events.emit("blog", "run.published", {
      subject: slug, data: { title: idea.title, topic: topic.key, run: runId },
    });
    bkn.lock.release(lock.key, lock.owner);
    return record(runId, "published", { slug: slug, title: idea.title, cover: cover });
  } catch (err) {
    const message = String(err && err.message || err);
    bkn.events.emit("blog", "run.failed", { level: "error", subject: runId, data: { error: message } });
    bkn.lock.release(lock.key, lock.owner);
    record(runId, "failed", { error: message });
    throw err;
  }
}
