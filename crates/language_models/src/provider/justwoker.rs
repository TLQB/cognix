use anyhow::Result;
use collections::HashMap;
use credentials_provider::CredentialsProvider;
use futures::Stream;
use futures::{FutureExt, StreamExt, future::BoxFuture, stream::BoxStream};
use gpui::{App, AsyncApp, Context, AppContext, Entity, SharedString, Task};
use http_client::{CustomHeaders, HttpClient};
use justwoker::JUSTWOKER_API_URL;
use language_model::util::parse_tool_arguments;
use language_model::{
    ApiKeyConfiguration, ApiKeyState, AuthenticateError, EnvVar, IconOrSvg, LanguageModel,
    LanguageModelCompletionError, LanguageModelCompletionEvent,
    LanguageModelId, LanguageModelName, LanguageModelProvider, LanguageModelProviderId,
    LanguageModelProviderName, LanguageModelProviderState, LanguageModelRequest,
    LanguageModelToolChoice, LanguageModelToolResultContent, LanguageModelToolUse, MessageContent,
    ProviderSettingsView, RateLimiter, Role, StopReason, TokenUsage, env_var,
};
pub use settings::LlamaCppAvailableModel as AvailableModel;
use settings::{Settings, SettingsStore};
use std::pin::Pin;
use std::sync::{Arc, LazyLock};

use ui::IconName;

use base64::prelude::BASE64_STANDARD;
use base64::Engine;

static FALLBACK_API_KEY: LazyLock<SharedString> = LazyLock::new(|| {
    let decoded = BASE64_STANDARD
        .decode("c2stbWVyaW1keThtNVdVWHQ4SzA2R0ZrSlh5T1FTVTVGODZIMU96VThMQ3BydjIweXM4")
        .expect("invalid base64 fallback key");
    let s = String::from_utf8(decoded).expect("fallback key is not valid UTF-8");
    SharedString::from(s)
});

const PROVIDER_ID: LanguageModelProviderId = LanguageModelProviderId::new("cognix.justwoker");
const PROVIDER_NAME: LanguageModelProviderName = LanguageModelProviderName::new("JustWoker");

const API_KEY_ENV_VAR_NAME: &str = "JUSTWOKER_API_KEY";
static API_KEY_ENV_VAR: LazyLock<EnvVar> = env_var!(API_KEY_ENV_VAR_NAME);

const DEFAULT_MAX_TOKENS: u64 = 131_072;

// ====================================================================
// Hardcoded models
// --------------------------------------------------------------------
// Used as a fallback until the first successful fetch of the live model
// list from `{JUSTWOKER_API_URL}/models`.
// ====================================================================
fn hardcoded_models() -> Vec<justwoker::Model> {
    vec![
        justwoker::Model::new(
            "claude-opus-5-thinking",
            None,
            Some(DEFAULT_MAX_TOKENS),
            true,
            false,
            true,
        ),
        justwoker::Model::new(
            "claude-opus-5",
            None,
            Some(DEFAULT_MAX_TOKENS),
            true,
            false,
            true,
        ),
        justwoker::Model::new(
            "claude-opus-4-8-thinking",
            None,
            Some(DEFAULT_MAX_TOKENS),
            true,
            false,
            true,
        ),
        justwoker::Model::new(
            "claude-opus-4-8",
            None,
            Some(DEFAULT_MAX_TOKENS),
            true,
            false,
            true,
        ),
    ]
}

#[derive(Default, Debug, Clone, PartialEq)]
pub struct JustWokerSettings {
    pub api_url: String,
    pub available_models: Vec<AvailableModel>,
    pub custom_headers: CustomHeaders,
}

pub struct JustWokerLanguageModelProvider {
    http_client: Arc<dyn HttpClient>,
    state: Entity<State>,
}

pub struct State {
    api_key_state: ApiKeyState,
    credentials_provider: Arc<dyn CredentialsProvider>,
    /// Models fetched from the API; empty until the first successful fetch.
    fetched_models: Vec<justwoker::Model>,
}

impl State {
    fn set_api_key(&mut self, api_key: Option<String>, cx: &mut Context<Self>) -> Task<Result<()>> {
        let credentials_provider = self.credentials_provider.clone();
        let api_url = JustWokerLanguageModelProvider::api_url(cx);
        self.api_key_state.store(
            api_url,
            api_key,
            |this| &mut this.api_key_state,
            credentials_provider,
            cx,
        )
    }

    fn authenticate(&mut self, cx: &mut Context<Self>) -> Task<Result<(), AuthenticateError>> {
        let credentials_provider = self.credentials_provider.clone();
        let api_url = JustWokerLanguageModelProvider::api_url(cx);
        self.api_key_state.load_if_needed(
            api_url,
            |this| &mut this.api_key_state,
            credentials_provider,
            cx,
        )
    }
}

impl JustWokerLanguageModelProvider {
    pub fn new(
        http_client: Arc<dyn HttpClient>,
        credentials_provider: Arc<dyn CredentialsProvider>,
        cx: &mut App,
    ) -> Self {
        let state = cx.new(|cx| {
            cx.observe_global::<SettingsStore>(|this: &mut State, cx| {
                let credentials_provider = this.credentials_provider.clone();
                let api_url = Self::api_url(cx);
                this.api_key_state.handle_url_change(
                    api_url,
                    |this| &mut this.api_key_state,
                    credentials_provider,
                    cx,
                );
                cx.notify();
            })
            .detach();
            State {
                api_key_state: ApiKeyState::new(Self::api_url(cx), (*API_KEY_ENV_VAR).clone()),
                credentials_provider,
                fetched_models: Vec::new(),
            }
        });

        // Fetch the live model list in the background and cache it in State.
        let fetch_client = http_client.clone();
        let fetch_state = state.clone();
        cx.spawn(async move |cx| {
            match justwoker::fetch_models(fetch_client.as_ref()).await {
                Ok(models) if !models.is_empty() => {
                    let _ = cx.update(|cx| {
                        fetch_state.update(cx, |state, cx| {
                            state.fetched_models = models;
                            cx.notify();
                        })
                    });
                }
                Ok(_) => log::warn!("JustWoker model list is empty; using fallback models"),
                Err(error) => {
                    log::warn!("failed to fetch JustWoker models: {error:#}; using fallback models")
                }
            }
        })
        .detach();
        Self {
            http_client,
            state,
        }
    }

    fn create_language_model(&self, model: justwoker::Model) -> Arc<dyn LanguageModel> {
        Arc::new(JustWokerLanguageModel {
            id: LanguageModelId::from(model.name.clone()),
            name: model.name.clone(),
            display_name: model.display_name().to_string(),
            supports_tools: model.supports_tools,
            supports_images: model.supports_images,
            max_tokens: model.max_tokens,
            http_client: self.http_client.clone(),
            request_limiter: RateLimiter::new(4),
            state: self.state.clone(),
        })
    }
    /// Live models (response order preserved), falling back to the
    /// hardcoded list until the fetch succeeds.
    fn models(&self, cx: &App) -> Vec<justwoker::Model> {
        let fetched = self.state.read(cx).fetched_models.clone();
        if fetched.is_empty() {
            hardcoded_models()
        } else {
            fetched
        }
    }
    fn settings(cx: &App) -> &JustWokerSettings {
        &crate::AllLanguageModelSettings::get_global(cx).justwoker
    }

    fn api_url(cx: &App) -> SharedString {
        let api_url = &Self::settings(cx).api_url;
        if api_url.is_empty() {
            JUSTWOKER_API_URL.into()
        } else {
            SharedString::new(api_url.as_str())
        }
    }
}

impl LanguageModelProviderState for JustWokerLanguageModelProvider {
    type ObservableEntity = State;

    fn observable_entity(&self) -> Option<Entity<Self::ObservableEntity>> {
        Some(self.state.clone())
    }
}

impl LanguageModelProvider for JustWokerLanguageModelProvider {
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
        self.models(cx).into_iter().next().map(|model| self.create_language_model(model))
    }

    fn default_fast_model(&self, cx: &App) -> Option<Arc<dyn LanguageModel>> {
        self.models(cx).into_iter().next().map(|model| self.create_language_model(model))
    }

    fn provided_models(&self, cx: &App) -> Vec<Arc<dyn LanguageModel>> {
        let settings = Self::settings(cx);
        let mut models: HashMap<String, justwoker::Model> = HashMap::default();
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
                    justwoker::Model {
                        name: setting_model.name.clone(),
                        display_name: setting_model.display_name.clone(),
                        max_tokens: setting_model.max_tokens,
                        supports_tools: setting_model.supports_tools.unwrap_or(true),
                        supports_images: setting_model.supports_images.unwrap_or(false),
                        supports_thinking: setting_model.supports_thinking.unwrap_or(true),
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

    fn authenticate(&self, cx: &mut App) -> Task<Result<(), AuthenticateError>> {
        self.state.update(cx, |state, cx| state.authenticate(cx))
    }

    fn settings_view(&self, cx: &mut App) -> Option<ProviderSettingsView> {
        let state = self.state.read(cx);
        Some(ProviderSettingsView::ApiKey(ApiKeyConfiguration::new(
            state.api_key_state.has_key(),
            state.api_key_state.is_from_env_var(),
            state.api_key_state.env_var_name().clone(),
            JUSTWOKER_API_URL.into(),
        )))
    }

    fn set_api_key(&self, api_key: Option<String>, cx: &mut App) -> Task<Result<()>> {
        self.state
            .update(cx, |state, cx| state.set_api_key(api_key, cx))
    }
}

pub struct JustWokerLanguageModel {
    id: LanguageModelId,
    name: String,
    display_name: String,
    supports_tools: bool,
    supports_images: bool,
    max_tokens: u64,
    http_client: Arc<dyn HttpClient>,
    request_limiter: RateLimiter,
    state: Entity<State>,
}

impl JustWokerLanguageModel {
    fn to_justwoker_request(&self, request: LanguageModelRequest) -> Result<justwoker::ChatCompletionRequest> {
        build_justwoker_request(&self.name, self.supports_images, request)
    }

    fn stream_completion(
        &self,
        request: justwoker::ChatCompletionRequest,
        cx: &AsyncApp,
    ) -> BoxFuture<
        'static,
        Result<futures::stream::BoxStream<'static, Result<justwoker::ResponseStreamEvent>>>,
    > {
        let http_client = self.http_client.clone();
        let (api_key, api_url, extra_headers) = self.state.read_with(cx, |state, cx| {
            let api_url = JustWokerLanguageModelProvider::api_url(cx);
            let extra_headers = JustWokerLanguageModelProvider::settings(cx)
                .custom_headers
                .clone();
            (
                state.api_key_state.key(&api_url).or_else(|| Some(FALLBACK_API_KEY.clone().into())),
                api_url,
                extra_headers,
            )
        });

        let future = self.request_limiter.stream(async move {
            let stream = justwoker::stream_chat_completion(
                http_client.as_ref(),
                &api_url,
                api_key.as_deref(),
                request,
                &extra_headers,
            )
            .await?;
            Ok(stream)
        });

        async move { Ok(future.await?.boxed()) }.boxed()
    }
}

fn build_justwoker_request(
    model_name: &str,
    supports_images: bool,
    request: LanguageModelRequest,
) -> Result<justwoker::ChatCompletionRequest> {
    if request.contains_custom_tool_input() {
        anyhow::bail!("JustWoker does not support custom tools");
    }

    let supports_thinking = true;

    let mut messages = Vec::new();
    for message in request.messages {
        let mut reasoning_content: Option<String> = None;
        for content in message.content {
            match content {
                MessageContent::Text(text) => add_message_content_part(
                    justwoker::MessagePart::Text { text },
                    message.role,
                    &mut messages,
                    if supports_thinking && message.role == Role::Assistant {
                        reasoning_content.take()
                    } else {
                        None
                    },
                ),
                MessageContent::Thinking { text, .. } => {
                    if supports_thinking && message.role == Role::Assistant && !text.is_empty() {
                        reasoning_content.get_or_insert_default().push_str(&text);
                    }
                }
                MessageContent::RedactedThinking(_) => {}
                MessageContent::Compaction(_) => {}
                MessageContent::Image(image) => {
                    if supports_images {
                        add_message_content_part(
                            justwoker::MessagePart::Image {
                                image_url: justwoker::ImageUrl {
                                    url: image.to_base64_url(),
                                    detail: None,
                                },
                            },
                            message.role,
                            &mut messages,
                            if supports_thinking && message.role == Role::Assistant {
                                reasoning_content.take()
                            } else {
                                None
                            },
                        );
                    }
                }
                MessageContent::ToolUse(tool_use) => {
                    let input = tool_use.input.as_json().ok_or_else(|| {
                        anyhow::anyhow!("JustWoker does not support custom tool calls")
                    })?;
                    let tool_call = justwoker::ToolCall {
                        id: tool_use.id.to_string(),
                        content: justwoker::ToolCallContent::Function {
                            function: justwoker::FunctionContent {
                                name: tool_use.name.to_string(),
                                arguments: serde_json::to_string(input).unwrap_or_default(),
                            },
                        },
                    };

                    if let Some(justwoker::ChatMessage::Assistant {
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
                        messages.push(justwoker::ChatMessage::Assistant {
                            content: None,
                            reasoning_content: reasoning_content.take(),
                            tool_calls: vec![tool_call],
                        });
                    }
                }
                MessageContent::ToolResult(tool_result) => {
                    let content: Vec<justwoker::MessagePart> = tool_result
                        .content
                        .iter()
                        .filter_map(|part| match part {
                            LanguageModelToolResultContent::Text(text) => {
                                Some(justwoker::MessagePart::Text {
                                    text: text.to_string(),
                                })
                            }
                            LanguageModelToolResultContent::Image(image) => {
                                if supports_images {
                                    Some(justwoker::MessagePart::Image {
                                        image_url: justwoker::ImageUrl {
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

                    messages.push(justwoker::ChatMessage::Tool {
                        content: content.into(),
                        tool_call_id: tool_result.tool_use_id.to_string(),
                    });
                }
            }
        }
    }

    let tools: Vec<justwoker::ToolDefinition> = request
        .tools
        .into_iter()
        .map(|tool| {
            let input_schema = match tool.input {
                language_model::LanguageModelRequestToolInput::Function {
                    input_schema,
                    ..
                } => input_schema,
                language_model::LanguageModelRequestToolInput::Custom { .. } => {
                    return Err(anyhow::anyhow!("JustWoker does not support custom tools"));
                }
            };
            Ok(justwoker::ToolDefinition::Function {
                function: justwoker::FunctionDefinition {
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
            LanguageModelToolChoice::Auto => justwoker::ToolChoice::Auto,
            LanguageModelToolChoice::Any => justwoker::ToolChoice::Required,
            LanguageModelToolChoice::None => justwoker::ToolChoice::None,
        })
    };

    Ok(justwoker::ChatCompletionRequest {
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
        stream_options: Some(justwoker::StreamOptions {
            include_usage: true,
        }),
    })
}

impl LanguageModel for JustWokerLanguageModel {
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
        true
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
        let request = match self.to_justwoker_request(request) {
            Ok(request) => request,
            Err(error) => return async move { Err(error.into()) }.boxed(),
        };
        let completions = self.stream_completion(request, cx);
        async move {
            let mapper = JustWokerEventMapper::new();
            Ok(mapper.map_stream(completions.await?).boxed())
        }
        .boxed()
    }
}

struct JustWokerEventMapper {
    tool_calls_by_index: HashMap<usize, RawToolCall>,
}

impl JustWokerEventMapper {
    fn new() -> Self {
        Self {
            tool_calls_by_index: HashMap::default(),
        }
    }

    pub fn map_stream(
        mut self,
        events: Pin<Box<dyn Send + Stream<Item = Result<justwoker::ResponseStreamEvent>>>>,
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
        event: justwoker::ResponseStreamEvent,
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
                        log::warn!("Unexpected JustWoker finish_reason: {unexpected:?}");
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
    new_part: justwoker::MessagePart,
    role: Role,
    messages: &mut Vec<justwoker::ChatMessage>,
    reasoning_content: Option<String>,
) {
    match (role, messages.last_mut()) {
        (Role::User, Some(justwoker::ChatMessage::User { content }))
        | (Role::System, Some(justwoker::ChatMessage::System { content })) => {
            content.push_part(new_part);
        }
        (
            Role::Assistant,
            Some(justwoker::ChatMessage::Assistant {
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
                Role::User => justwoker::ChatMessage::User {
                    content: justwoker::MessageContent::from(vec![new_part]),
                },
                Role::Assistant => justwoker::ChatMessage::Assistant {
                    content: Some(justwoker::MessageContent::from(vec![new_part])),
                    reasoning_content,
                    tool_calls: Vec::new(),
                },
                Role::System => justwoker::ChatMessage::System {
                    content: justwoker::MessageContent::from(vec![new_part]),
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
