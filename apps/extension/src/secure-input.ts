export {};

const requestId = new URL(location.href).searchParams.get("requestId") ?? "";
const form = document.querySelector<HTMLFormElement>("#form")!;
const input = document.querySelector<HTMLInputElement>("#value")!;
const label = document.querySelector<HTMLLabelElement>("#field-label")!;
const description = document.querySelector<HTMLElement>("#description")!;

void chrome.runtime
  .sendMessage({ source: "secure-input", type: "get", requestId })
  .then((request) => {
    if (!request) {
      form.remove();
      description.textContent = "This secure input request is no longer valid.";
      return;
    }
    label.textContent = request.label || "Value";
    input.type = request.secret === false ? "text" : "password";
    input.autocomplete = request.autocomplete || "off";
    description.textContent = `Fill ${request.label || "the requested field"} on ${request.origin}. The value stays inside the extension.`;
    input.focus();
  });

form.addEventListener("submit", (event) => {
  event.preventDefault();
  void chrome.runtime
    .sendMessage({
      source: "secure-input",
      type: "submit",
      requestId,
      value: input.value,
    })
    .then(() => window.close());
});
document.querySelector("#cancel")!.addEventListener("click", () => {
  void chrome.runtime
    .sendMessage({ source: "secure-input", type: "cancel", requestId })
    .then(() => window.close());
});
