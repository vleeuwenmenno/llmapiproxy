// ChatV2 Alpine.js component — placeholder skeleton
// Full implementation will be added by frontend workers.

function chatApp() {
    return {
        sessions: [],
        messages: [],
        input: '',
        streaming: false,
        selectedModel: '',
        sidebarOpen: false,
        status: 'ready',

        init() {
            const data = window.__CHATV2_DATA__ || {};
            this.selectedModel = data.DefaultModel || 'gpt-4o';

            // Load sessions from API
            fetch('/ui/chatv2/sessions')
                .then(r => r.json())
                .then(sessions => { this.sessions = sessions || []; })
                .catch(() => {});
        },

        sendMessage() {
            if (!this.input.trim()) return;
            // Full streaming implementation will be added by frontend workers.
        }
    };
}
