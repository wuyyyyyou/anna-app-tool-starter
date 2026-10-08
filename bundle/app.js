import { AnnaAppRuntime } from "/static/anna-apps/_sdk/latest/index.js";

const button = document.getElementById("run");
const status = document.getElementById("status");
const fields = ["string_value", "integer_value", "number_value", "boolean_value", "array_value", "object_value"];

function showError(error) {
  status.classList.add("error");
  status.textContent = `Call failed: ${error.message}${error.code ? ` (${error.code})` : ""}`;
}

async function main() {
  const toolId = window.__ANNA_TOOL_IDS__?.["type-demo"];
  if (!toolId) {
    throw new Error("Missing tool ID mapping. Open the app through anna-app dev or the Anna platform.");
  }

  const anna = await AnnaAppRuntime.connect();
  button.disabled = false;
  status.textContent = "Connected. Click the button to get data.";

  button.addEventListener("click", async () => {
    button.disabled = true;
    status.classList.remove("error");
    status.textContent = "Calling the backend…";
    for (const field of fields) document.getElementById(field).textContent = "—";
    document.getElementById("json").textContent = "Waiting for a response…";

    try {
      // The SDK returns the business payload. It handles the RPC envelopes.
      const data = await anna.tools.invoke({
        tool_id: toolId,
        method: "get_types",
        args: {},
      });
      for (const field of fields) {
        if (!Object.hasOwn(data, field)) throw new Error(`Backend response is missing a field: ${field}`);
      }
      for (const field of fields) {
        document.getElementById(field).textContent = JSON.stringify(data[field], null, 2);
      }
      document.getElementById("json").textContent = JSON.stringify(data, null, 2);
      status.textContent = "Received six data types from the Go backend.";
    } catch (error) {
      document.getElementById("json").textContent = "This call returned no data.";
      showError(error);
    } finally {
      button.disabled = false;
    }
  });
}

main().catch(showError);
