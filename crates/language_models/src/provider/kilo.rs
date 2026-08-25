use anyhow::Result;
use collections::HashMap;
use futures::{FutureExt, StreamExt, Stream, future::BoxFuture, stream::BoxStream};
use gpui::{App, AsyncApp, AppContext, Entity, SharedString, Task};
use http_client::{CustomHeaders, HttpClient};
use language_model::util::parse_tool_arguments;
use language_model::{
    AuthenticateError, IconOrSvg, LanguageModel, LanguageModelCompletionError,
    LanguageModelCompletionEvent, LanguageModelEffortLevel, LanguageModelId, LanguageModelName,
    LanguageModelProvider, LanguageModelProviderId, LanguageModelProviderName,
    LanguageModelProviderState, LanguageModelRequest, LanguageModelToolChoice,
    LanguageModelToolResultContent, LanguageModelToolUse, MessageContent, ProviderSettingsView,
    RateLimiter, Role, StopReason, TokenUsage,
};
pub use settings::LlamaCppAvailableModel as AvailableModel;
use settings::{Settings, SettingsStore};
use std::pin::Pin;
use std::sync::Arc;
use kilo::KILO_API_URL;

use ui::IconName;

const PROVIDER_ID: LanguageModelProviderId = LanguageModelProviderId::new("cognix.kilo");
const PROVIDER_NAME: LanguageModelProviderName = LanguageModelProviderName::new("Cognix-Kilo");

// ====================================================================
// Reasoning-effort configuration
// --------------------------------------------------------------------
// Only models that advertise `reasoning_effort` in their supported
// parameters receive the parameter on the wire; for everyone else it is
// omitted entirely.
// ====================================================================

const REASONING_EFFORT_LEVELS: &[(&str, &str)] = &[
    ("low", "Low"),
    ("medium", "Medium"),
    ("high", "High"),
];

const DEFAULT_REASONING_EFFORT: &str = "medium";

fn supported_effort_levels() -> Vec<LanguageModelEffortLevel> {
    REASONING_EFFORT_LEVELS
        .iter()
        .map(|(value, label)| LanguageModelEffortLevel {
            name: (*label).into(),
            value: (*value).into(),
            is_default: *value == DEFAULT_REASONING_EFFORT,
        })
        .collect()
}

fn resolve_reasoning_effort(request: &LanguageModelRequest) -> Option<String> {
    if !request.thinking_allowed {
        return None;
    }

    let chosen = request
        .thinking_effort
        .as_deref()
        .filter(|effort| REASONING_EFFORT_LEVELS.iter().any(|(v, _)| *v == *effort))
        .unwrap_or(DEFAULT_REASONING_EFFORT);

    Some(chosen.to_string())
}

// ====================================================================
// Hardcoded models
// --------------------------------------------------------------------
// Free models are discovered from the gateway registry at startup; this
// list only covers the window before the first successful fetch (or its
// failure, e.g. while offline).
// ====================================================================
fn hardcoded_models() -> Vec<kilo::Model> {
    vec![
        kilo::Model {
            name: "nvidia/nemotron-3-super-120b-a12b:free".to_string(),
            display_name: None,
            max_tokens: 262_144,
            supports_tools: true,
            supports_images: false,
            supports_thinking: true,
            supports_reasoning_effort: true,
        },
        kilo::Model {
            name: "stepfun/step-3.7-flash:free".to_string(),
            display_name: None,
            max_tokens: 262_144,
            supports_tools: true,
            supports_images: true,
            supports_thinking: true,
            supports_reasoning_effort: false,
        },
    ]
}

#[derive(Default, Debug, Clone, PartialEq)]
pub struct KiloSettings {
    pub api_url: String,
    pub available_models: Vec<AvailableModel>,
    pub custom_headers: CustomHeaders,
}

pub struct KiloLanguageModelProvider {
    http_client: Arc<dyn HttpClient>,
    state: Entity<State>,
}

pub struct State {
    /// Free models from the gateway registry; empty until the first
    /// successful fetch.
    fetched_models: Vec<kilo::Model>,
}

impl KiloLanguageModelProvider {
    pub fn new(http_client: Arc<dyn HttpClient>, cx: &mut App) -> Self {
        let state = cx.new(|cx| {
            cx.observe_global::<SettingsStore>(|_this: &mut State, cx| {
                cx.notify();
            })
            .detach();
            State {
                fetched_models: Vec::new(),
            }
        });

        // Fetch the free-model registry in the background and cache it in State.
        let fetch_client = http_client.clone();
        let fetch_state = state.clone();
        cx.spawn(async move |cx| {
            match kilo::fetch_models(fetch_client.as_ref()).await {
                Ok(models) if !models.is_empty() => {
                    let _ = cx.update(|cx| {
                        fetch_state.update(cx, |state, cx| {
                            state.fetched_models = models;
                            cx.notify();
                        })
                    });
                }
                Ok(_) => log::warn!("Kilo model registry returned no free models; using fallback models"),
                Err(error) => {
                    log::warn!("failed to fetch Kilo models: {error:#}; using fallback models")
                }
            }
        })
        .detach();
        Self { http_client, state }
    }

    fn create_language_model(&self, model: kilo::Model) -> Arc<dyn LanguageModel> {
        Arc::new(KiloLanguageModel {
            id: LanguageModelId::from(model.name.clone()),
            name: model.name.clone(),
            display_name: model.display_name().to_string(),
            supports_tools: model.supports_tools,
            supports_images: model.supports_images,
            supports_thinking: model.supports_thinking,
            supports_reasoning_effort: model.supports_reasoning_effort,
            max_tokens: model.max_tokens,
            http_client: self.http_client.clone(),
            request_limiter: RateLimiter::new(4),
            state: self.state.clone(),
        })
    }

    /// Registry models (response order preserved), falling back to the
    /// hardcoded list until the fetch succeeds.
    fn models(&self, cx: &App) -> Vec<kilo::Model> {
        let fetched = self.state.read(cx).fetched_models.clone();
        if fetched.is_empty() {
            hardcoded_models()
        } else {
            fetched
        }
    }

    fn settings(cx: &App) -> &KiloSettings {
        &crate::AllLanguageModelSettings::get_global(cx).kilo
    }

    fn api_url(cx: &App) -> SharedString {
        let api_url = &Self::settings(cx).api_url;
        if api_url.is_empty() {
            KILO_API_URL.into()
        } else {
            SharedString::new(api_url.as_str())
        }
    }
}

impl LanguageModelProviderState for KiloLanguageModelProvider {
    type ObservableEntity = State;

    fn observable_entity(&self) -> Option<Entity<Self::ObservableEntity>> {
        Some(self.state.clone())
    }
}

impl LanguageModelProvider for KiloLanguageModelProvider {
    fn id(&self) -> LanguageModelProviderId {
        PROVIDER_ID
    }

    fn name(&self) -> LanguageModelProviderName {
        PROVIDER_NAME
    }

    fn icon(&self) -> IconOrSvg {
        IconOrSvg::Icon(IconName::Cognix)
    }

    fn default_model(&self, cx: &App) -> Option<Arc<dyn LanguageModel>> {
        self.models(cx)
            .into_iter()
            .next()
            .map(|model| self.create_language_model(model))
    }

    fn default_fast_model(&self, cx: &App) -> Option<Arc<dyn LanguageModel>> {
        self.models(cx)
            .into_iter()
            .next()
            .map(|model| self.create_language_model(model))
    }

    fn provided_models(&self, cx: &App) -> Vec<Arc<dyn LanguageModel>> {
        let settings = Self::settings(cx);
        let mut models: HashMap<String, kilo::Model> = HashMap::default();
        for model in self.models(cx) {
            models.insert(model.name.clone(), model);
        }

        for setting_model in &settings.available_models {
            if let Some(model) = models.get_mut(&setting_model.name) {
                if setting_model.display_name.is_some() {
                    model.display_name = setting_model.display_name.clone();
                }
                if let Some(supports_tools) = setting_model.supports_tools {
                    model.supports_tools = supports_tools;
                }
                if let Some(supports_images) = setting_model.supports_images {
                    model.supports_images = supports_images;
                }
                if let Some(supports_thinking) = setting_model.supports_thinking {
                    model.supports_thinking = supports_thinking;
                }
                model.max_tokens = setting_model.max_tokens;
            } else {
                models.insert(
                    setting_model.name.clone(),
                    kilo::Model {
                        name: setting_model.name.clone(),
                        display_name: setting_model.display_name.clone(),
                        max_tokens: setting_model.max_tokens,
                        supports_tools: setting_model.supports_tools.unwrap_or(true),
                        supports_images: setting_model.supports_images.unwrap_or(false),
                        supports_thinking: setting_model.supports_thinking.unwrap_or(true),
                        supports_reasoning_effort: true,
                    },
                );
            }
        }

        let mut models = models.into_values().collect::<Vec<_>>();
        models.sort_by_key(|model| model.name.clone());
        models
            .into_iter()
            .map(|model| self.create_language_model(model))
            .collect()
    }

    fn is_authenticated(&self, _cx: &App) -> bool {
        true
    }

    fn authenticate(&self, _cx: &mut App) -> Task<Result<(), AuthenticateError>> {
        // Kilo's free models require no credentials.
        Task::ready(Ok(()))
    }

    fn settings_view(&self, _cx: &mut App) -> Option<ProviderSettingsView> {
        None
    }

    fn set_api_key(&self, _api_key: Option<String>, _cx: &mut App) -> Task<Result<()>> {
        // Kilo's free models require no credentials.
        Task::ready(Ok(()))
    }
}

pub struct KiloLanguageModel {
    id: LanguageModelId,
    name: String,
    display_name: String,
    supports_tools: bool,
    supports_images: bool,
    supports_thinking: bool,
    supports_reasoning_effort: bool,
    max_tokens: u64,
    http_client: Arc<dyn HttpClient>,
    request_limiter: RateLimiter,
    state: Entity<State>,
}

impl KiloLanguageModel {
    fn to_kilo_request(&self, request: LanguageModelRequest) -> Result<kilo::ChatCompletionRequest> {
        build_kilo_request(
            &self.name,
            self.supports_images,
            self.supports_reasoning_effort,
            request,
        )
    }

    fn stream_completion(
        &self,
        request: kilo::ChatCompletionRequest,
        cx: &AsyncApp,
    ) -> BoxFuture<
        'static,
        Result<futures::stream::BoxStream<'static, Result<kilo::ResponseStreamEvent>>>,
    > {
        let http_client = self.http_client.clone();
        let (api_url, extra_headers) = self.state.read_with(cx, |_, cx| {
            (
                KiloLanguageModelProvider::api_url(cx),
                KiloLanguageModelProvider::settings(cx)
                    .custom_headers
                    .clone(),
            )
        });

        let future = self.request_limiter.stream(async move {
            let stream =
                kilo::stream_chat_completion(http_client.as_ref(), &api_url, request, &extra_headers)
                    .await?;
            Ok(stream)
        });

        async move { Ok(future.await?.boxed()) }.boxed()
    }
}

fn build_kilo_request(
    model_name: &str,
    supports_images: bool,
    supports_reasoning_effort: bool,
    request: LanguageModelRequest,
) -> Result<kilo::ChatCompletionRequest> {
    if request.contains_custom_tool_input() {
        anyhow::bail!("Kilo does not support custom tools");
    }

    let reasoning_effort = resolve_reasoning_effort(&request).filter(|_| supports_reasoning_effort);

    let mut messages = Vec::new();
    for message in request.messages {
        let mut reasoning_content: Option<String> = None;
        for content in message.content {
            match content {
                MessageContent::Text(text) => add_message_content_part(
                    kilo::MessagePart::Text { text },
                    message.role,
                    &mut messages,
                    if message.role == Role::Assistant {
                        reasoning_content.take()
                    } else {
                        None
                    },
                ),
                MessageContent::Thinking { text, .. } => {
                    if message.role == Role::Assistant && !text.is_empty() {
                        reasoning_content.get_or_insert_default().push_str(&text);
                    }
                }
                MessageContent::RedactedThinking(_) => {}
                MessageContent::Compaction(_) => {}
                MessageContent::Image(image) => {
                    if supports_images {
                        add_message_content_part(
                            kilo::MessagePart::Image {
                                image_url: kilo::ImageUrl {
                                    url: image.to_base64_url(),
                                    detail: None,
                                },
                            },
                            message.role,
                            &mut messages,
                            None,
                        );
                    }
                }
                MessageContent::ToolUse(tool_use) => {
                    let input = tool_use.input.as_json().ok_or_else(|| {
                        anyhow::anyhow!("Kilo does not support custom tool calls")
                    })?;
                    let tool_call = kilo::ToolCall {
                        id: tool_use.id.to_string(),
                        content: kilo::ToolCallContent::Function {
                            function: kilo::FunctionContent {
                                name: tool_use.name.to_string(),
                                arguments: serde_json::to_string(input).unwrap_or_default(),
                            },
                        },
                    };

                    if let Some(kilo::ChatMessage::Assistant {
                        tool_calls,
                        reasoning_content: message_reasoning_content,
                        ..
                    }) = messages.last_mut()
                    {
                        append_reasoning_content(
                            message_reasoning_content,
                            reasoning_content.take(),
                        );
                        tool_calls.push(tool_call);
                    } else {
                        messages.push(kilo::ChatMessage::Assistant {
                            content: None,
                            reasoning_content: reasoning_content.take(),
                            tool_calls: vec![tool_call],
                        });
                    }
                }
                MessageContent::ToolResult(tool_result) => {
                    let content: Vec<kilo::MessagePart> = tool_result
                        .content
                        .iter()
                        .filter_map(|part| match part {
                            LanguageModelToolResultContent::Text(text) => {
                                Some(kilo::MessagePart::Text {
                                    text: text.to_string(),
                                })
                            }
                            LanguageModelToolResultContent::Image(image) => {
                                if supports_images {
                                    Some(kilo::MessagePart::Image {
                                        image_url: kilo::ImageUrl {
                                            url: image.to_base64_url(),
                                            detail: None,
                                        },
                                    })
                                } else {
                                    None
                                }
                            }
                        })
                        .collect();

                    messages.push(kilo::ChatMessage::Tool {
                        content: content.into(),
                        tool_call_id: tool_result.tool_use_id.to_string(),
                    });
                }
            }
        }
    }

    let tools: Vec<kilo::ToolDefinition> = request
        .tools
        .into_iter()
        .map(|tool| {
            let input_schema = match tool.input {
                language_model::LanguageModelRequestToolInput::Function {
                    input_schema,
                    ..
                } => input_schema,
                language_model::LanguageModelRequestToolInput::Custom { .. } => {
                    return Err(anyhow::anyhow!("Kilo does not support custom tools"));
                }
            };
            Ok(kilo::ToolDefinition::Function {
                function: kilo::FunctionDefinition {
                    name: tool.name,
                    description: Some(tool.description),
                    parameters: Some(input_schema),
                },
            })
        })
        .collect::<Result<_>>()?;
    let tool_choice = if tools.is_empty() {
        None
    } else {
        request.tool_choice.map(|choice| match choice {
            LanguageModelToolChoice::Auto => kilo::ToolChoice::Auto,
            LanguageModelToolChoice::Any => kilo::ToolChoice::Required,
            LanguageModelToolChoice::None => kilo::ToolChoice::None,
        })
    };

    Ok(kilo::ChatCompletionRequest {
        model: model_name.to_string(),
        messages,
        stream: true,
        max_tokens: None,
        stop: if request.stop.is_empty() {
            None
        } else {
            Some(request.stop)
        },
        temperature: request.temperature,
        tools,
        tool_choice,
        stream_options: Some(kilo::StreamOptions {
            include_usage: true,
        }),
        reasoning_effort,
    })
}

impl LanguageModel for KiloLanguageModel {
    fn id(&self) -> LanguageModelId {
        self.id.clone()
    }

    fn name(&self) -> LanguageModelName {
        LanguageModelName::from(self.display_name.clone())
    }

    fn provider_id(&self) -> LanguageModelProviderId {
        PROVIDER_ID
    }

    fn provider_name(&self) -> LanguageModelProviderName {
        PROVIDER_NAME
    }

    fn supports_tools(&self) -> bool {
        self.supports_tools
    }

    fn supports_tool_choice(&self, choice: LanguageModelToolChoice) -> bool {
        self.supports_tools()
            && match choice {
                LanguageModelToolChoice::Auto => true,
                LanguageModelToolChoice::Any => true,
                LanguageModelToolChoice::None => true,
            }
    }

    fn supports_images(&self) -> bool {
        self.supports_images
    }

    fn supports_thinking(&self) -> bool {
        self.supports_thinking
    }

    fn supported_effort_levels(&self) -> Vec<LanguageModelEffortLevel> {
        supported_effort_levels()
    }

    fn telemetry_id(&self) -> String {
        format!("{PROVIDER_ID}/{}", self.name)
    }

    fn max_token_count(&self) -> u64 {
        self.max_tokens
    }

    fn stream_completion(
        &self,
        request: LanguageModelRequest,
        cx: &AsyncApp,
    ) -> BoxFuture<
        'static,
        Result<
            BoxStream<'static, Result<LanguageModelCompletionEvent, LanguageModelCompletionError>>,
            LanguageModelCompletionError,
        >,
    > {
        let request = match self.to_kilo_request(request) {
            Ok(request) => request,
            Err(error) => return async move { Err(error.into()) }.boxed(),
        };
        let completions = self.stream_completion(request, cx);
        async move {
            let mapper = KiloEventMapper::new();
            Ok(mapper.map_stream(completions.await?).boxed())
        }
        .boxed()
    }
}

struct KiloEventMapper {
    tool_calls_by_index: HashMap<usize, RawToolCall>,
}

impl KiloEventMapper {
    fn new() -> Self {
        Self {
            tool_calls_by_index: HashMap::default(),
        }
    }

    pub fn map_stream(
        mut self,
        events: Pin<Box<dyn Send + Stream<Item = Result<kilo::ResponseStreamEvent>>>>,
    ) -> impl Stream<Item = Result<LanguageModelCompletionEvent, LanguageModelCompletionError>>
    {
        events.flat_map(move |event| {
            futures::stream::iter(match event {
                Ok(event) => self.map_event(event),
                Err(error) => vec![Err(LanguageModelCompletionError::from(error))],
            })
        })
    }

    pub fn map_event(
        &mut self,
        event: kilo::ResponseStreamEvent,
    ) -> Vec<Result<LanguageModelCompletionEvent, LanguageModelCompletionError>> {
        let mut events = Vec::new();

        if let Some(usage) = event.usage {
            events.push(Ok(LanguageModelCompletionEvent::UsageUpdate(TokenUsage {
                input_tokens: usage.prompt_tokens,
                output_tokens: usage.completion_tokens,
                cache_creation_input_tokens: 0,
                cache_read_input_tokens: 0,
            })));
        }

        if let Some(choice) = event.choices.into_iter().next() {
            if let Some(reasoning_content) = choice.delta.reasoning_content {
                events.push(Ok(LanguageModelCompletionEvent::Thinking {
                    text: reasoning_content,
                    signature: None,
                }));
            }

            if let Some(content) = choice.delta.content {
                if !content.is_empty() {
                    events.push(Ok(LanguageModelCompletionEvent::Text(content)));
                }
            }

            if let Some(tool_calls) = choice.delta.tool_calls {
                for tool_call in tool_calls {
                    let entry = self.tool_calls_by_index.entry(tool_call.index).or_default();

                    if let Some(tool_id) = tool_call.id {
                        entry.id = tool_id;
                    }

                    if let Some(function) = tool_call.function {
                        if let Some(name) = function.name {
                            if !name.is_empty() {
                                entry.name = name;
                            }
                        }

                        if let Some(arguments) = function.arguments {
                            entry.arguments.push_str(&arguments);
                        }
                    }
                }
            }

            if let Some(finish_reason) = choice.finish_reason.as_deref() {
                match finish_reason {
                    "stop" => {
                        events.push(Ok(LanguageModelCompletionEvent::Stop(StopReason::EndTurn)));
                    }
                    "tool_calls" => {
                        events.extend(self.tool_calls_by_index.drain().map(|(_, tool_call)| {
                            match parse_tool_arguments(&tool_call.arguments) {
                                Ok(input) => Ok(LanguageModelCompletionEvent::ToolUse(
                                    LanguageModelToolUse {
                                        id: tool_call.id.into(),
                                        name: tool_call.name.into(),
                                        is_input_complete: true,
                                        input: language_model::LanguageModelToolUseInput::Json(
                                            input,
                                        ),
                                        raw_input: tool_call.arguments,
                                        thought_signature: None,
                                    },
                                )),
                                Err(error) => {
                                    Ok(LanguageModelCompletionEvent::ToolUseJsonParseError {
                                        id: tool_call.id.into(),
                                        tool_name: tool_call.name.into(),
                                        raw_input: tool_call.arguments.into(),
                                        json_parse_error: error.to_string(),
                                    })
                                }
                            }
                        }));

                        events.push(Ok(LanguageModelCompletionEvent::Stop(StopReason::ToolUse)));
                    }
                    "length" => {
                        events.push(Ok(LanguageModelCompletionEvent::Stop(
                            StopReason::MaxTokens,
                        )));
                    }
                    unexpected => {
                        log::warn!("Unexpected Kilo finish_reason: {unexpected:?}");
                        events.push(Ok(LanguageModelCompletionEvent::Stop(StopReason::EndTurn)));
                    }
                }
            }
        }

        events
    }
}

#[derive(Default)]
struct RawToolCall {
    id: String,
    name: String,
    arguments: String,
}

fn add_message_content_part(
    new_part: kilo::MessagePart,
    role: Role,
    messages: &mut Vec<kilo::ChatMessage>,
    reasoning_content: Option<String>,
) {
    match (role, messages.last_mut()) {
        (Role::User, Some(kilo::ChatMessage::User { content }))
        | (Role::System, Some(kilo::ChatMessage::System { content })) => {
            content.push_part(new_part);
        }
        (
            Role::Assistant,
            Some(kilo::ChatMessage::Assistant {
                content: Some(content),
                reasoning_content: message_reasoning_content,
                ..
            }),
        ) => {
            append_reasoning_content(message_reasoning_content, reasoning_content);
            content.push_part(new_part);
        }
        _ => {
            messages.push(match role {
                Role::User => kilo::ChatMessage::User {
                    content: kilo::MessageContent::from(vec![new_part]),
                },
                Role::Assistant => kilo::ChatMessage::Assistant {
                    content: Some(kilo::MessageContent::from(vec![new_part])),
                    reasoning_content,
                    tool_calls: Vec::new(),
                },
                Role::System => kilo::ChatMessage::System {
                    content: kilo::MessageContent::from(vec![new_part]),
                },
            });
        }
    }
}

fn append_reasoning_content(target: &mut Option<String>, content: Option<String>) {
    let Some(content) = content else {
        return;
    };
    if content.is_empty() {
        return;
    }
    target.get_or_insert_default().push_str(&content);
}
