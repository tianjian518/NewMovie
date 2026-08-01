/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        ink: "#0f1115",
        panel: "#171b22",
        card: "#1b1f27",
        brand: "#1f6feb",
      },
    },
  },
  plugins: [],
};
