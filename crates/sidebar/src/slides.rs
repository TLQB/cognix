//! "Generate Slides" — drive the embedded proxy's /v1/slides endpoint.
//!
//! The sidecar (zai-proxy on 127.0.0.1:3001) exposes POST /v1/slides: an
//! OpenAI-style request in, an SSE stream of progress events out, ending in
//! a deck event with one standalone HTML page per slide plus a global
//! stylesheet. The sidecar also exposes /v1/slides/export/pptx, a
//! passthrough to chat.z.ai's html-to-ppt conversion that accepts exactly
//! that deck shape.
//!
//! This module wires it into the editor: the agents_sidebar::GenerateSlides
//! action takes a topic from the clipboard (copy the text describing the
//! deck first), runs the generation, writes a standalone preview HTML into
//! the first project directory, exports the .pptx next to it (best effort),
//! and reports both paths in a toast.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Context as _;
use futures::{AsyncBufReadExt, AsyncReadExt, StreamExt, io::BufReader};
use gpui::AppContext as _;
use http_client::{AsyncBody, HttpClient};
use serde::Deserialize;
use workspace::Workspace;

/// Base URL of the embedded sidecar.
const SLIDES_BASE_URL: &str = "http://127.0.0.1:3001";
/// Built-in sidecar auth password (mirrors the glm provider wiring).
const PROXY_PASSWORD: &str = "Waguri";

gpui::actions!(
    agents_sidebar,
    [
        /// Generates an HTML slide deck (plus a best-effort .pptx export)
        /// from the clipboard contents as the topic.
        GenerateSlides,
    ]
);

// ---------------------------------------------------------------- deck model

/// One page of a generated deck (mirror of the sidecar's deck event).
#[derive(Deserialize, Clone, Debug)]
pub struct Slide {
    pub position: usize,
    pub title: String,
    pub html: String,
}

/// Final deck event from /v1/slides.
#[derive(Deserialize, Clone, Debug)]
pub struct SlideDeckEvent {
    pub conversation_id: String,
    pub slides: Vec<Slide>,
    #[serde(default)]
    pub global_css: String,
}

#[derive(Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum SlidesEvent {
    #[serde(rename = "progress")]
    Progress {},
    #[serde(rename = "op")]
    Op {},
    #[serde(rename = "op_error")]
    OpError {
        #[allow(dead_code)]
        error: String,
    },
    #[serde(rename = "error")]
    Error {
        error: String,
    },
    Deck(SlideDeckEvent),
}

// --------------------------------------------------------------- generation

/// POSTs the authoring request and consumes the SSE stream until the final
/// deck event (or an error event).
async fn collect_deck(
    client: Arc<dyn HttpClient>,
    topic: String,
) -> anyhow::Result<SlideDeckEvent> {
    let body = serde_json::json!({
        "model": "glm-5.3-flash",
        "stream": true,
        "conversation_id": format!(
            "zagent-slides-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)?
                .as_millis()
        ),
        "messages": [{"role": "user", "content": topic}],
    })
    .to_string();

    let request = http::Request::builder()
        .method(http::Method::POST)
        .uri(format!("{SLIDES_BASE_URL}/v1/slides"))
        .header("Content-Type", "application/json")
        .header("Authorization", format!("Bearer {PROXY_PASSWORD}"))
        .body(AsyncBody::from(body))?;

    let response = client.send(request).await?;
    if !response.status().is_success() {
        anyhow::bail!("slides endpoint returned {}", response.status());
    }

    let reader = BufReader::new(response.into_body());
    let mut lines = reader.lines();
    let mut last_error = None;
    while let Some(line) = lines.next().await {
        let line = line?;
        let Some(payload) = line.strip_prefix("data: ") else {
            continue;
        };
        if payload == "[DONE]" {
            break;
        }
        match serde_json::from_str::<SlidesEvent>(payload) {
            Ok(SlidesEvent::Deck(deck)) => return Ok(deck),
            Ok(SlidesEvent::Error { error }) => last_error = Some(error),
            Ok(_) => {}
            Err(_) => {}
        }
    }
    Err(anyhow::anyhow!(
        last_error.unwrap_or_else(|| "no deck event received".to_string())
    ))
}

// ------------------------------------------------------------------- export

/// POSTs the deck to the sidecar's html-to-ppt passthrough and returns the
/// pptx bytes. Best-effort by design: when the conversion service is
/// unreachable the caller degrades to the HTML preview.
async fn export_pptx(
    client: Arc<dyn HttpClient>,
    deck: &SlideDeckEvent,
    filename: &str,
) -> anyhow::Result<Vec<u8>> {
    let html: Vec<_> = deck.slides.iter().map(|s| s.html.clone()).collect();
    let css = vec![deck.global_css.clone()];
    let body = serde_json::json!({
        "chatId": deck.conversation_id,
        "versionId": "v1",
        "upload": false,
        "filename": filename,
        "files": {"html": html, "css": css},
    })
    .to_string();

    let request = http::Request::builder()
        .method(http::Method::POST)
        .uri(format!("{SLIDES_BASE_URL}/v1/slides/export/pptx"))
        .header("Content-Type", "application/json")
        .header("Authorization", format!("Bearer {PROXY_PASSWORD}"))
        .body(AsyncBody::from(body))?;

    let mut response = client.send(request).await?;
    let mut bytes = Vec::new();
    response.body_mut().read_to_end(&mut bytes).await?;
    if !response.status().is_success() {
        anyhow::bail!(
            "export endpoint returned {}: {}",
            response.status(),
            String::from_utf8_lossy(&bytes)
        );
    }
    Ok(bytes)
}

// -------------------------------------------------------------------- HTML

/// Renders a standalone preview document: one fixed-size page per slide,
/// vertically stacked, with the deck's global stylesheet inlined.
pub fn render_preview_html(deck: &SlideDeckEvent) -> String {
    let mut pages = String::new();
    for slide in &deck.slides {
        pages.push_str(&format!(
            "\n<div class=\"page-wrap\">\n{}\n</div>\n",
            slide.html
        ));
    }
    let title = deck
        .slides
        .first()
        .map(|s| html_escape(&s.title))
        .unwrap_or_else(|| "Slides".to_string());
    format!(
		"<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>{title}</title>\n<style>\nbody {{ background: #2a2a2e; margin: 0; padding: 24px; display: flex; flex-direction: column; align-items: center; gap: 24px; }}
.page-wrap {{ width: 1280px; height: 720px; overflow: hidden; box-shadow: 0 8px 32px rgba(0,0,0,.45); background: #fff; }}
{global_css}
</style>\n</head>\n<body>{pages}</body>\n</html>\n",
		title = title,
		global_css = deck.global_css,
		pages = pages,
	)
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

// ------------------------------------------------------------------ action

/// Action handler (registered on Workspace): the clipboard is the topic.
pub fn generate_slides(
    workspace: &mut Workspace,
    _: &GenerateSlides,
    _window: &mut gpui::Window,
    cx: &mut gpui::Context<Workspace>,
) {
    struct SlidesNotification;
    let notification_id = workspace::notifications::NotificationId::unique::<SlidesNotification>();
    let topic = current_topic(cx);
    if topic.is_empty() {
        workspace.show_toast(
			workspace::Toast::new(
				notification_id,
				"No topic: copy the text describing the deck you want, then run Generate Slides again.",
			)
			.autohide(),
			cx,
		);
        return;
    }

    let out_dir = workspace
        .project()
        .read(cx)
        .visible_worktrees(cx)
        .next()
        .map(|worktree| PathBuf::from(worktree.read(cx).abs_path().as_ref()))
        .unwrap_or_else(|| PathBuf::from("."));

    let client: Arc<dyn HttpClient> = workspace.app_state().client.http_client();

    let pipeline = cx.background_spawn(async move {
        let deck = collect_deck(client.clone(), topic).await?;

        std::fs::create_dir_all(&out_dir)
            .with_context(|| format!("create output dir {:?}", out_dir))?;

        let html_path = out_dir.join("slides-preview.html");
        std::fs::write(&html_path, render_preview_html(&deck))
            .with_context(|| format!("write {:?}", html_path))?;

        let pptx_path = match export_pptx(client, &deck, "presentation.pptx").await {
            Ok(bytes) => {
                let pptx_path = out_dir.join("slides.pptx");
                std::fs::write(&pptx_path, &bytes)
                    .with_context(|| format!("write {:?}", pptx_path))?;
                Some(pptx_path)
            }
            Err(err) => {
                log::warn!("pptx export unavailable, keeping HTML preview: {err:#}");
                None
            }
        };
        Ok::<_, anyhow::Error>((html_path, pptx_path, deck.slides.len()))
    });

    cx.spawn(async move |workspace, cx| {
        let result = pipeline.await;
        workspace
			.update(cx, |workspace, cx| match result {
				Ok((html_path, pptx_path, count)) => {
					let summary = match pptx_path {
						Some(pptx) => format!(
							"Generated {count} slides.\nPreview: {}\nPPTX: {}",
							html_path.display(),
							pptx.display()
						),
						None => format!(
							"Generated {count} slides.\nPreview: {}\nPPTX export unavailable (see log).",
							html_path.display()
						),
					};
					workspace.show_toast(workspace::Toast::new(notification_id, summary).autohide(), cx);
				}
				Err(err) => {
					workspace.show_toast(
						workspace::Toast::new(notification_id, format!("Slides failed: {err:#}")).autohide(),
						cx,
					);
				}
			})
			.ok();
    })
    .detach();
}

fn current_topic(cx: &gpui::App) -> String {
    cx.read_from_clipboard()
        .and_then(|item| item.text())
        .map(|text| text.trim().to_string())
        .unwrap_or_default()
}
