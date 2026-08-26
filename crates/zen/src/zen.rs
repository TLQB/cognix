use anyhow::{Context as _, Result, anyhow};
use futures::{AsyncBufReadExt, AsyncReadExt, StreamExt, io::BufReader, stream::BoxStream};
use http_client::{AsyncBody, CustomHeaders, HttpClient, Method, RequestBuilderExt, http};
use rand::Rng;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::sync::LazyLock;

pub const ZEN_API_URL: &str = "https://opencode.ai/zen/v1";

/// Anonymous access key accepted by the upstream. It only permits free-tier
/// models (ids ending in `-free`); paid models require a real key from
/// <https://opencode.ai/zen/>.
pub const PUBLIC_API_KEY: &str = "public";

const DEFAULT_CONTEXT_LENGTH: u64 = 131_072;
const CLIENT_NAME: &str = "cli";
const USER_AGENT: &str = "opencode/0.0.0";

/// Stable per-process session id required by the upstream wire protocol.
static SESSION_ID: LazyLock<String> = LazyLock::new(|| format!("sess_{}", hex_id(24)));
/// Stable per-process project id required by the upstream wire protocol.
static PROJECT_ID: LazyLock<String> = LazyLock::new(|| format!("prj_{}", hex_id(16)));

/// Random lowercase hex string of the given length.
fn hex_id(num_chars: usize) -> String {
    let mut result = String::with_capacity(num_chars + 15);
    while result.len() < num_chars {
        let value: u64 = rand::rng().random();
        result.push_str(&format!("{value:016x}"));
    }
    result.truncate(num_chars);
    result
}

/// Headers the upstream expects on every request. Omitting any of these makes
/// the upstream treat the client as a bot and reject it.
fn build_request_headers(api_key: &str, is_stream: bool) -> Result<http::HeaderMap> {
    let mut headers = http::HeaderMap::new();
    headers.insert(
        http::header::AUTHORIZATION,
        http::HeaderValue::from_str(&format!("Bearer {api_key}"))?,
    );
    headers.insert(http::header::CONTENT_TYPE, http::HeaderValue::from_static("application/json"));
    headers.insert(
        http::header::ACCEPT,
        if is_stream {
            http::HeaderValue::from_static("text/event-stream")
        } else {
            http::HeaderValue::from_static("application/json")
        },
    );
    headers.insert(
        http::HeaderName::from_static("x-opencode-session"),
        http::HeaderValue::from_str(&SESSION_ID)?,
    );
    headers.insert(
        http::HeaderName::from_static("x-opencode-request"),
        http::HeaderValue::from_str(&format!("req_{}", hex_id(24)))?,
    );
    headers.insert(
        http::HeaderName::from_static("x-opencode-project"),
        http::HeaderValue::from_str(&PROJECT_ID)?,
    );
    headers.insert(
        http::HeaderName::from_static("x-opencode-client"),
        http::HeaderValue::from_static(CLIENT_NAME),
    );
    // Overrides the HTTP client's default User-Agent; reqwest only fills in
    // defaults for headers the request did not set.
    headers.insert(
        http::header::USER_AGENT,
        http::HeaderValue::from_static(USER_AGENT),
    );
    Ok(headers)
}

/// A model exposed to the rest of Zed.
#[cfg_attr(feature = "schemars", derive(schemars::JsonSchema))]
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq)]
pub struct Model {
    pub name: String,
    pub display_name: Option<String>,
    pub max_tokens: u64,
    pub supports_tools: bool,
    pub supports_images: bool,
    pub supports_thinking: bool,
}

impl Model {
    pub fn new(
        name: &str,
        display_name: Option<&str>,
        max_tokens: Option<u64>,
        supports_tools: bool,
        supports_images: bool,
        supports_thinking: bool,
    ) -> Self {
        Self {
            name: name.to_owned(),
            display_name: display_name.map(ToString::to_string),
            max_tokens: max_tokens.unwrap_or(DEFAULT_CONTEXT_LENGTH),
            supports_tools,
            supports_images,
            supports_thinking,
        }
    }

    pub fn display_name(&self) -> &str {
        self.display_name.as_deref().unwrap_or(&self.name)
    }
}

/// Whether a model id is accessible with the anonymous `public` key.
pub fn is_free_model(name: &str) -> bool {
    name.ends_with("-free")
}

#[derive(Debug, Deserialize)]
struct ModelListResponse {
    #[serde(default)]
    data: Vec<ModelEntry>,
}

#[derive(Debug, Deserialize)]
struct ModelEntry {
    id: String,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ToolChoice {
    Auto,
    Required,
    None,
}

#[derive(Clone, Deserialize, Serialize, Debug)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ToolDefinition {
    Function { function: FunctionDefinition },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct FunctionDefinition {
    pub name: String,
    pub description: Option<String>,
    pub parameters: Option<Value>,
}

#[derive(Serialize, Deserialize, Debug)]
#[serde(tag = "role", rename_all = "lowercase")]
pub enum ChatMessage {
    Assistant {
        #[serde(default)]
        content: Option<MessageContent>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        reasoning_content: Option<String>,
        #[serde(default, skip_serializing_if = "Vec::is_empty")]
        tool_calls: Vec<ToolCall>,
    },
    User {
        content: MessageContent,
    },
    System {
        content: MessageContent,
    },
    Tool {
        content: MessageContent,
        tool_call_id: String,
    },
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
#[serde(untagged)]
pub enum MessageContent {
    Plain(String),
    Multipart(Vec<MessagePart>),
}

impl MessageContent {
    pub fn push_part(&mut self, part: MessagePart) {
        match self {
            MessageContent::Plain(text) => {
                *self =
                    MessageContent::Multipart(vec![MessagePart::Text { text: text.clone() }, part]);
            }
            MessageContent::Multipart(parts) if parts.is_empty() => match part {
                MessagePart::Text { text } => *self = MessageContent::Plain(text),
                MessagePart::Image { .. } => *self = MessageContent::Multipart(vec![part]),
            },
            MessageContent::Multipart(parts) => parts.push(part),
        }
    }
}

impl From<Vec<MessagePart>> for MessageContent {
    fn from(mut parts: Vec<MessagePart>) -> Self {
        if let [MessagePart::Text { text }] = parts.as_mut_slice() {
            MessageContent::Plain(std::mem::take(text))
        } else {
            MessageContent::Multipart(parts)
        }
    }
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum MessagePart {
    Text {
        text: String,
    },
    #[serde(rename = "image_url")]
    Image {
        image_url: ImageUrl,
    },
}

#[derive(Serialize, Deserialize, Clone, Debug, Eq, PartialEq)]
pub struct ImageUrl {
    pub url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
pub struct ToolCall {
    pub id: String,
    #[serde(flatten)]
    pub content: ToolCallContent,
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum ToolCallContent {
    Function { function: FunctionContent },
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
pub struct FunctionContent {
    pub name: String,
    pub arguments: String,
}

#[derive(Serialize, Debug)]
pub struct ChatCompletionRequest {
    pub model: String,
    pub messages: Vec<ChatMessage>,
    pub stream: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_tokens: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stop: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ToolDefinition>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<ToolChoice>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stream_options: Option<StreamOptions>,
}

/// Asks the server to include a final `usage` chunk in the stream.
#[derive(Serialize, Debug)]
pub struct StreamOptions {
    pub include_usage: bool,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct Usage {
    pub prompt_tokens: u64,
    pub completion_tokens: u64,
    pub total_tokens: u64,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ZenError {
    pub message: String,
}

#[derive(Serialize, Deserialize, Debug)]
#[serde(untagged)]
pub enum ResponseStreamResult {
    Ok(ResponseStreamEvent),
    Err { error: ZenError },
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ResponseStreamEvent {
    pub model: String,
    pub object: String,
    pub choices: Vec<ChoiceDelta>,
    pub usage: Option<Usage>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ChoiceDelta {
    pub index: u32,
    pub delta: ResponseMessageDelta,
    pub finish_reason: Option<String>,
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
pub struct ResponseMessageDelta {
    pub content: Option<String>,
    /// Reasoning models emit their chain of thought in a dedicated
    /// `reasoning_content` field.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_content: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_calls: Option<Vec<ToolCallChunk>>,
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
pub struct ToolCallChunk {
    pub index: usize,
    pub id: Option<String>,
    pub function: Option<FunctionChunk>,
}

#[derive(Serialize, Deserialize, Debug, Eq, PartialEq)]
pub struct FunctionChunk {
    pub name: Option<String>,
    pub arguments: Option<String>,
}

/// Lists the models exposed by the upstream (`GET {api_url}/models`).
pub async fn fetch_models(
    client: &dyn HttpClient,
    api_url: &str,
    api_key: &str,
    extra_headers: &CustomHeaders,
) -> Result<Vec<Model>> {
    let uri = format!("{api_url}/models");
    let mut builder = http::Request::builder().method(Method::GET).uri(uri);
    if let Some(request_headers) = builder.headers_mut() {
        *request_headers = build_request_headers(api_key, false)?;
    }
    let request = builder
        .extra_headers(extra_headers)
        .body(AsyncBody::empty())?;

    let mut response = client.send(request).await?;
    let mut body = String::new();
    response.body_mut().read_to_string(&mut body).await?;
    if !response.status().is_success() {
        anyhow::bail!(
            "failed to fetch Zen models: {} {}",
            response.status(),
            body
        );
    }
    let response: ModelListResponse =
        serde_json::from_str(&body).context("failed to parse Zen models list")?;

    Ok(response
        .data
        .into_iter()
        .filter(|entry| {
            // Keep models that end with "-free" OR are exactly "big-pickle"
            entry.id.ends_with("-free") || entry.id == "big-pickle"
        })
        .map(|entry| {
            let display_name = if entry.id.ends_with("-free") {
                // Remove the "-free" suffix for display
                Some(entry.id.trim_end_matches("-free").to_string())
            } else {
                None
            };
            
            Model {
                name: entry.id,
                display_name,
                max_tokens: DEFAULT_CONTEXT_LENGTH,
                supports_tools: true,
                supports_images: false,
                supports_thinking: true,
            }
        })
        .collect())
}
/// Streams a chat completion from the upstream (`POST {api_url}/chat/completions`).
pub async fn stream_chat_completion(
    client: &dyn HttpClient,
    api_url: &str,
    api_key: &str,
    request: ChatCompletionRequest,
    extra_headers: &CustomHeaders,
) -> Result<BoxStream<'static, Result<ResponseStreamEvent>>> {
    let uri = format!("{api_url}/chat/completions");
    let mut builder = http::Request::builder().method(Method::POST).uri(uri);
    if let Some(request_headers) = builder.headers_mut() {
        *request_headers = build_request_headers(api_key, true)?;
    }
    let request_builder = builder.extra_headers(extra_headers);

    let request =
        request_builder.body(AsyncBody::from(serde_json::to_string(&request)?))?;
    let mut response = client.send(request).await?;
    if response.status().is_success() {
        let reader = BufReader::new(response.into_body());
        Ok(reader
            .lines()
            .filter_map(|line| async move {
                match line {
                    Ok(line) => {
                        let line = line.strip_prefix("data: ")?;
                        if line == "[DONE]" {
                            None
                        } else {
                            match serde_json::from_str(line) {
                                Ok(ResponseStreamResult::Ok(response)) => Some(Ok(response)),
                                Ok(ResponseStreamResult::Err { error }) => {
                                    Some(Err(anyhow!(error.message)))
                                }
                                Err(error) => Some(Err(anyhow!(error))),
                            }
                        }
                    }
                    Err(error) => Some(Err(anyhow!(error))),
                }
            })
            .boxed())
    } else {
        let mut body = String::new();
        response.body_mut().read_to_string(&mut body).await?;
        anyhow::bail!(
            "Failed to connect to Zen API: {} {}",
            response.status(),
            body,
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generates_hex_ids_of_requested_length() {
        let id = hex_id(24);
        assert_eq!(id.len(), 24);
        assert!(id.chars().all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()));

        let session = format!("sess_{}", hex_id(24));
        assert_eq!(session.len(), "sess_".len() + 24);
        assert!(session.starts_with("sess_"));

        let other = hex_id(24);
        assert_ne!(id, other);
    }

    #[test]
    fn builds_all_required_wire_headers() {
        let headers = build_request_headers(PUBLIC_API_KEY, true).unwrap();

        assert_eq!(
            headers.get(http::header::AUTHORIZATION).unwrap(),
            "Bearer public"
        );
        assert_eq!(
            headers.get(http::header::CONTENT_TYPE).unwrap(),
            "application/json"
        );
        assert_eq!(
            headers.get(http::header::ACCEPT).unwrap(),
            "text/event-stream"
        );
        let session = headers.get("x-opencode-session").unwrap().to_str().unwrap();
        assert!(session.starts_with("sess_"));
        assert_eq!(session.len(), 24 + 5);
        let request_id = headers.get("x-opencode-request").unwrap().to_str().unwrap();
        assert!(request_id.starts_with("req_"));
        assert_eq!(request_id.len(), 24 + 4);
        let project = headers.get("x-opencode-project").unwrap().to_str().unwrap();
        assert!(project.starts_with("prj_"));
        assert_eq!(project.len(), 16 + 4);
        assert_eq!(headers.get("x-opencode-client").unwrap(), "cli");
        assert_eq!(headers.get(http::header::USER_AGENT).unwrap(), "opencode/0.0.0");

        let non_stream_headers = build_request_headers("sk-test", false).unwrap();
        assert_eq!(
            non_stream_headers.get(http::header::ACCEPT).unwrap(),
            "application/json"
        );
        assert_eq!(
            non_stream_headers.get(http::header::AUTHORIZATION).unwrap(),
            "Bearer sk-test"
        );
        // Session and project stay stable across requests.
        assert_eq!(
            non_stream_headers.get("x-opencode-session").unwrap(),
            session
        );
        assert_eq!(
            non_stream_headers.get("x-opencode-project").unwrap(),
            project
        );
    }

    #[test]
    fn identifies_free_models() {
        assert!(is_free_model("deepseek-v4-flash-free"));
        assert!(!is_free_model("claude-opus-5"));
    }

    #[test]
    fn parses_models_list() {
        let payload = r#"{
            "data": [
                {"id": "deepseek-v4-flash-free"},
                {"id": "mimo-v2.5-free"}
            ]
        }"#;
        let response: ModelListResponse = serde_json::from_str(payload).unwrap();
        assert_eq!(response.data.len(), 2);
        assert_eq!(response.data[0].id, "deepseek-v4-flash-free");
    }

    #[test]
    fn parses_streaming_reasoning_and_tool_calls() {
        let event = serde_json::json!({
            "model": "deepseek-v4-flash-free",
            "object": "chat.completion.chunk",
            "choices": [
                {
                    "index": 0,
                    "delta": {
                        "role": "assistant",
                        "content": null,
                        "reasoning_content": "thinking...",
                        "tool_calls": [
                            {
                                "index": 0,
                                "id": "call_1",
                                "function": { "name": "weather", "arguments": "{\"city\":" }
                            }
                        ]
                    },
                    "finish_reason": null
                }
            ]
        });
        let event: ResponseStreamEvent = serde_json::from_value(event).unwrap();
        let delta = &event.choices[0].delta;
        assert_eq!(delta.reasoning_content.as_deref(), Some("thinking..."));
        assert_eq!(delta.tool_calls.as_ref().unwrap().len(), 1);
    }

    #[test]
    fn parses_streaming_error_envelope() {
        let payload = r#"{"error": {"message": "rate limited"}}"#;
        let result: ResponseStreamResult = serde_json::from_str(payload).unwrap();
        match result {
            ResponseStreamResult::Err { error } => assert_eq!(error.message, "rate limited"),
            ResponseStreamResult::Ok(_) => panic!("expected error envelope"),
        }
    }
}
