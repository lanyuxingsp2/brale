const overlay = document.getElementById('modal-overlay');
const perspectiveContainer = document.getElementById('modal-perspective');
const bookModal = document.getElementById('central-book-modal');
const chapterTitle = document.getElementById('book-chapter-title');
const contentLeft = document.getElementById('book-content-left');
const contentRight = document.getElementById('book-content-right');

// 打开书本
// title: 标题
// themeColor: 'blue' | 'red'
// section: 'decisions' | 'positions' (用于后续加载数据)
function openBook(title, themeColor, section) {
    chapterTitle.textContent = title;
    
    // 重置样式
    chapterTitle.className = chapterTitle.className.replace(/text-\w+-\d+/g, 'text-slate-800');
    bookModal.style.borderColor = '#475569';

    // 设置主题色
    if (themeColor === 'blue') {
        chapterTitle.classList.add('text-blue-900');
        bookModal.style.borderColor = '#1e3a8a';
    } else if (themeColor === 'red') {
        chapterTitle.classList.add('text-red-900');
        bookModal.style.borderColor = '#991b1b';
    }

    // 显示模态框容器
    overlay.classList.remove('hidden');
    perspectiveContainer.classList.remove('hidden');
    
    // 强制重排，确保 transition 生效
    void overlay.offsetWidth; 

    overlay.classList.remove('opacity-0');
    overlay.classList.add('opacity-100');
    
    setTimeout(() => {
        bookModal.classList.add('active');
    }, 50);

    // 模拟加载数据 (在后端接口就绪前)
    loadMockData(section);
}

function closeBook() {
    bookModal.classList.remove('active');
    overlay.classList.remove('opacity-100');
    overlay.classList.add('opacity-0');

    setTimeout(() => {
        overlay.classList.add('hidden');
        perspectiveContainer.classList.add('hidden');
    }, 600); // 等待动画结束
}

// 模拟数据加载，供审阅使用
function loadMockData(section) {
    // 清空现有内容
    contentLeft.innerHTML = '<div class="animate-pulse p-4">Loading archives...</div>';
    contentRight.innerHTML = '';

    setTimeout(() => {
        if (section === 'decisions') {
            renderDecisions();
        } else if (section === 'positions') {
            renderPositions();
        }
    }, 300);
}

function renderDecisions() {
    contentLeft.innerHTML = `
        <div class="space-y-6">
            <div class="border-l-4 border-blue-500 pl-4 py-2 bg-blue-50/50">
                <div class="flex justify-between text-sm text-slate-500">
                    <span>2023-10-27 14:30:05</span>
                    <span class="font-mono">ETH/USDT</span>
                </div>
                <h4 class="text-lg font-bold text-slate-800 mt-1">LONG Entry Signal</h4>
                <p class="text-sm text-slate-600 mt-1">AI Confidence: <span class="text-green-600 font-bold">85%</span></p>
                <p class="text-sm mt-2">RSI oversold divergence detected on 15m timeframe. Volume increasing.</p>
            </div>
            
            <div class="border-l-4 border-slate-300 pl-4 py-2 hover:bg-slate-50 transition-colors cursor-pointer">
                <div class="flex justify-between text-sm text-slate-500">
                    <span>2023-10-27 13:15:00</span>
                    <span class="font-mono">BTC/USDT</span>
                </div>
                <h4 class="text-lg font-bold text-slate-700 mt-1">HOLD</h4>
                <p class="text-sm text-slate-600 mt-1">Market consolidating. No clear direction.</p>
            </div>

             <div class="border-l-4 border-slate-300 pl-4 py-2 hover:bg-slate-50 transition-colors cursor-pointer">
                <div class="flex justify-between text-sm text-slate-500">
                    <span>2023-10-27 12:00:00</span>
                    <span class="font-mono">SOL/USDT</span>
                </div>
                <h4 class="text-lg font-bold text-slate-700 mt-1">SHORT Exit</h4>
                <p class="text-sm text-slate-600 mt-1">Profit target reached. Closing position.</p>
            </div>
        </div>
    `;

    contentRight.innerHTML = `
        <div class="bg-slate-100 p-4 rounded text-xs font-mono text-slate-600 border border-slate-200">
            <p class="mb-2 font-bold text-slate-800">// Raw Analysis Data</p>
            <p>{"rsi": 28.5, "ema_200": 1850.2, "trend": "bullish"}</p>
            <br>
            <p class="text-blue-600">>> Executing Order: BUY ETH/USDT</p>
        </div>
    `;
}

function renderPositions() {
    contentLeft.innerHTML = `
        <table class="w-full text-left border-collapse">
            <thead>
                <tr class="text-slate-500 text-sm border-b border-slate-300">
                    <th class="pb-2 font-normal">Pair</th>
                    <th class="pb-2 font-normal">Open Price</th>
                    <th class="pb-2 font-normal text-right">P&L %</th>
                </tr>
            </thead>
            <tbody class="text-slate-700">
                <tr class="border-b border-slate-200/50 hover:bg-red-50/50">
                    <td class="py-3 font-mono font-bold">BTC/USDT</td>
                    <td class="py-3 font-mono text-sm">34,500.00</td>
                    <td class="py-3 font-mono text-right text-green-600">+1.25%</td>
                </tr>
                <tr class="border-b border-slate-200/50 hover:bg-red-50/50">
                    <td class="py-3 font-mono font-bold">ETH/USDT</td>
                    <td class="py-3 font-mono text-sm">1,780.50</td>
                    <td class="py-3 font-mono text-right text-red-500">-0.45%</td>
                </tr>
                 <tr class="border-b border-slate-200/50 hover:bg-red-50/50">
                    <td class="py-3 font-mono font-bold">XRP/USDT</td>
                    <td class="py-3 font-mono text-sm">0.5520</td>
                    <td class="py-3 font-mono text-right text-green-600">+0.10%</td>
                </tr>
            </tbody>
        </table>
    `;

    contentRight.innerHTML = `
        <div class="p-6 bg-red-50 border border-red-100 rounded text-center">
            <h3 class="text-red-900 font-bold mb-2">Total P&L</h3>
            <p class="text-4xl font-mono text-green-600 font-bold">+$125.40</p>
            <p class="text-sm text-red-900/50 mt-2">24h Change</p>
        </div>
        <div class="mt-8">
             <h3 class="text-slate-800 font-bold mb-4">Active Bots</h3>
             <div class="flex items-center gap-3">
                <div class="w-3 h-3 rounded-full bg-green-500 animate-pulse"></div>
                <span class="text-sm text-slate-600">Freqtrade Worker #1 (Running)</span>
            </div>
        </div>
        `;
}

// 支持在 admin 侧 0-100% 输入分批比例，提交前自动转换为 0-1
document.addEventListener('submit', (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    if (form.dataset.tierForm !== 'true') return;
    const ratioFields = ['tier1_ratio', 'tier2_ratio', 'tier3_ratio'];
    ratioFields.forEach((name) => {
        const input = form.querySelector(`input[name="${name}"]`);
        if (!input || input.value === '') return;
        const val = parseFloat(input.value);
        if (Number.isNaN(val)) return;
        if (val > 1) {
            const pct = Math.min(val, 100);
            input.value = (pct / 100).toFixed(4);
        }
    });
});

// 根据鼠标位置微调大书本的倾斜方向
function applyBookTilt(maxTilt = 1.5) {
    const books = document.querySelectorAll('.big-open-book');
    books.forEach((book) => {
        book.addEventListener('mousemove', (e) => {
            const rect = book.getBoundingClientRect();
            const x = e.clientX - rect.left;
            const y = e.clientY - rect.top;
            const tiltX = ((rect.height / 2 - y) / rect.height) * maxTilt;
            const tiltY = ((x - rect.width / 2) / rect.width) * maxTilt;
            book.style.transform = `rotateX(${tiltX}deg) rotateY(${tiltY}deg)`;
        });
        book.addEventListener('mouseleave', () => {
            book.style.transform = '';
        });
    });
}

document.addEventListener('DOMContentLoaded', () => applyBookTilt());
